// Package api implements the Ari Media Engine REST control plane: stream
// management, embedding-security configuration, aggregate stats and health.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/johndesu090/ari-media-engine/internal/broadcast"
	"github.com/johndesu090/ari-media-engine/internal/config"
	"github.com/johndesu090/ari-media-engine/internal/delivery"
	"github.com/johndesu090/ari-media-engine/internal/webhook"
	"github.com/johndesu090/ari-media-engine/internal/webrtc"
	"github.com/johndesu090/ari-media-engine/pkg/id"
)

// Options configures the API server.
type Options struct {
	Store     broadcast.Store
	Engine    *webrtc.Engine
	Policy    *delivery.DomainPolicy
	Logger    *slog.Logger
	Webhooks  *webhook.Dispatcher
	Auth      config.AuthConfig
	StartedAt time.Time
}

// Server exposes the REST API.
type Server struct {
	store     broadcast.Store
	engine    *webrtc.Engine
	policy    *delivery.DomainPolicy
	log       *slog.Logger
	webhooks  *webhook.Dispatcher
	auth      config.AuthConfig
	startedAt time.Time
}

// New constructs the API server.
func New(opts Options) *Server {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	startedAt := opts.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	return &Server{
		store:     opts.Store,
		engine:    opts.Engine,
		policy:    opts.Policy,
		log:       log,
		webhooks:  opts.Webhooks,
		auth:      opts.Auth,
		startedAt: startedAt,
	}
}

// Routes returns a standalone handler serving the API with auth applied.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	s.Register(mux)
	return mux
}

// Register installs the API routes into an existing mux so a single root mux
// can serve the API, player, signaling and dashboard together. Authentication
// is applied per control-plane route so public endpoints stay reachable.
func (s *Server) Register(mux *http.ServeMux) {
	protect := func(h http.HandlerFunc) http.Handler { return s.requireAuth(h) }

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.Handle("GET /api/v1/stats", protect(s.handleStats))

	mux.Handle("GET /api/v1/streams", protect(s.handleListStreams))
	mux.Handle("POST /api/v1/streams", protect(s.handleCreateStream))
	mux.Handle("GET /api/v1/streams/{id}", protect(s.handleGetStream))
	mux.Handle("DELETE /api/v1/streams/{id}", protect(s.handleDeleteStream))
	mux.Handle("GET /api/v1/streams/{id}/stats", protect(s.handleStreamStats))

	mux.Handle("GET /api/v1/security/domains", protect(s.handleGetGlobalDomains))
	mux.Handle("PUT /api/v1/security/domains", protect(s.handlePutGlobalDomains))
	mux.Handle("GET /api/v1/security/streams/{id}/domains", protect(s.handleGetStreamDomains))
	mux.Handle("PUT /api/v1/security/streams/{id}/domains", protect(s.handlePutStreamDomains))
}

// requireAuth enforces the configured API key on control-plane routes.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	if !s.auth.Enabled || s.auth.APIKey == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		key := r.Header.Get("X-API-Key")
		if key == "" {
			if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
				key = strings.TrimPrefix(auth, "Bearer ")
			}
		}
		if key != s.auth.APIKey {
			writeError(w, http.StatusUnauthorized, "unauthorized", "a valid API key is required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

/* ------------------------------- health/stats ------------------------------ */

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) handleStats(w http.ResponseWriter, _ *http.Request) {
	streams, err := s.store.List(rctx())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "storage_error", err.Error())
		return
	}
	live := 0
	viewers := 0
	for _, b := range streams {
		if b.Status() == broadcast.StatusLive {
			live++
		}
		viewers += b.ViewerCount()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"uptimeSeconds": int(time.Since(s.startedAt).Seconds()),
		"startedAt":     s.startedAt,
		"streamsTotal":  len(streams),
		"streamsLive":   live,
		"viewers":       viewers,
		"engineStreams": s.engine.StreamCount(),
	})
}

/* --------------------------------- streams -------------------------------- */

func (s *Server) handleListStreams(w http.ResponseWriter, _ *http.Request) {
	streams, err := s.store.List(rctx())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "storage_error", err.Error())
		return
	}
	out := make([]broadcast.Snapshot, 0, len(streams))
	for _, b := range streams {
		out = append(out, b.Snapshot())
	}
	writeJSON(w, http.StatusOK, map[string]any{"streams": out, "count": len(out)})
}

type createStreamRequest struct {
	StreamID string            `json:"streamId"`
	Name     string            `json:"name"`
	Type     string            `json:"type"`
	Metadata map[string]string `json:"metadata"`
}

func (s *Server) handleCreateStream(w http.ResponseWriter, r *http.Request) {
	var req createStreamRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	streamID := strings.TrimSpace(req.StreamID)
	if streamID == "" {
		streamID = id.New("stream")
	}
	btype := broadcast.Type(strings.ToLower(strings.TrimSpace(req.Type)))
	if btype == "" {
		btype = broadcast.TypeWebRTC
	}
	if !validType(btype) {
		writeError(w, http.StatusBadRequest, "invalid_type", "type must be one of webrtc, rtmp, srt, rtsp")
		return
	}
	name := req.Name
	if name == "" {
		name = streamID
	}

	b := broadcast.New(id.New("bc"), streamID, name, btype)
	if req.Metadata != nil {
		b.SetMetadata(req.Metadata)
	}
	if err := s.store.Create(rctx(), b); err != nil {
		if errors.Is(err, broadcast.ErrExists) {
			writeError(w, http.StatusConflict, "stream_exists", "a stream with that id already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "storage_error", err.Error())
		return
	}

	s.emit(webhook.EventStreamCreated, streamID, nil)
	writeJSON(w, http.StatusCreated, s.snapshot(b))
}

func (s *Server) handleGetStream(w http.ResponseWriter, r *http.Request) {
	b, err := s.store.Get(rctx(), r.PathValue("id"))
	if err != nil {
		s.writeLookupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.snapshot(b))
}

func (s *Server) handleDeleteStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, err := s.store.Get(rctx(), id)
	if err != nil {
		s.writeLookupError(w, err)
		return
	}
	if err := s.store.Delete(rctx(), id); err != nil {
		s.writeLookupError(w, err)
		return
	}
	s.policy.DeleteStream(b.StreamID())
	s.emit(webhook.EventStreamDeleted, b.StreamID(), nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStreamStats(w http.ResponseWriter, r *http.Request) {
	b, err := s.store.Get(rctx(), r.PathValue("id"))
	if err != nil {
		s.writeLookupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stream":       s.snapshot(b),
		"viewerCount":  s.engine.ViewerCount(b.StreamID()),
		"engineLive":   s.engine.HasLiveStream(b.StreamID()),
		"protocol":     "webrtc",
		"embedDomains": s.policy.Effective(b.StreamID()),
	})
}

/* -------------------------------- security -------------------------------- */

type domainsRequest struct {
	AllowedDomains []string `json:"allowedDomains"`
	AllowAll       *bool    `json:"allowAll"`
}

func (s *Server) handleGetGlobalDomains(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"scope":          "global",
		"allowAll":       s.policy.AllowAll(),
		"allowedDomains": s.policy.Global(),
	})
}

func (s *Server) handlePutGlobalDomains(w http.ResponseWriter, r *http.Request) {
	var req domainsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if req.AllowAll != nil {
		s.policy.SetAllowAll(*req.AllowAll)
	}
	s.policy.SetGlobal(req.AllowedDomains)
	writeJSON(w, http.StatusOK, map[string]any{
		"scope":          "global",
		"allowAll":       s.policy.AllowAll(),
		"allowedDomains": s.policy.Global(),
	})
}

func (s *Server) handleGetStreamDomains(w http.ResponseWriter, r *http.Request) {
	streamID := s.resolveStreamID(r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]any{
		"scope":          "stream",
		"streamId":       streamID,
		"allowAll":       s.policy.AllowAll(),
		"allowedDomains": s.policy.Stream(streamID),
		"effective":      s.policy.Effective(streamID),
	})
}

func (s *Server) handlePutStreamDomains(w http.ResponseWriter, r *http.Request) {
	streamID := s.resolveStreamID(r.PathValue("id"))
	var req domainsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	s.policy.SetStream(streamID, req.AllowedDomains)
	writeJSON(w, http.StatusOK, map[string]any{
		"scope":          "stream",
		"streamId":       streamID,
		"allowAll":       s.policy.AllowAll(),
		"allowedDomains": s.policy.Stream(streamID),
		"effective":      s.policy.Effective(streamID),
	})
}

// resolveStreamID maps a broadcast id to its stream key when one exists, so
// callers may use either identifier in security routes.
func (s *Server) resolveStreamID(idOrStream string) string {
	if b, err := s.store.Get(rctx(), idOrStream); err == nil {
		return b.StreamID()
	}
	return idOrStream
}

/* --------------------------------- helpers -------------------------------- */

func (s *Server) snapshot(b *broadcast.Broadcast) broadcast.Snapshot {
	snap := b.Snapshot()
	snap.ViewerCount = s.engine.ViewerCount(b.StreamID())
	return snap
}

func (s *Server) writeLookupError(w http.ResponseWriter, err error) {
	if errors.Is(err, broadcast.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "stream not found")
		return
	}
	writeError(w, http.StatusInternalServerError, "storage_error", err.Error())
}

func (s *Server) emit(eventType, streamID string, data map[string]any) {
	if s.webhooks == nil {
		return
	}
	s.webhooks.Emit(webhook.Event{Type: eventType, StreamID: streamID, Data: data})
}

func validType(t broadcast.Type) bool {
	switch t {
	case broadcast.TypeWebRTC, broadcast.TypeRTMP, broadcast.TypeSRT, broadcast.TypeRTSP:
		return true
	default:
		return false
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": code, "message": message})
}

// rctx returns the context used for store lookups. The in-memory store ignores
// it; a networked store may bind a deadline here in a future revision.
func rctx() context.Context {
	return context.Background()
}
