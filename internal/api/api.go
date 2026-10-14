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
	"github.com/johndesu090/ari-media-engine/internal/cluster"
	"github.com/johndesu090/ari-media-engine/internal/config"
	"github.com/johndesu090/ari-media-engine/internal/delivery"
	"github.com/johndesu090/ari-media-engine/internal/storage"
	"github.com/johndesu090/ari-media-engine/internal/webhook"
	"github.com/johndesu090/ari-media-engine/internal/webrtc"
	"github.com/johndesu090/ari-media-engine/pkg/id"
)

// Options configures the API server.
type Options struct {
	Store        broadcast.Store
	Engine       *webrtc.Engine
	Policy       *delivery.DomainPolicy
	Signer       *delivery.TokenSigner
	Logger       *slog.Logger
	Webhooks     *webhook.Dispatcher
	Auth         config.AuthConfig
	StartedAt    time.Time
	Transcodes   func() int
	HLSBase      string
	DashBase     string
	Cluster      *cluster.Manager
	Recordings   storage.Store
	RecordActive func() int
}

// Server exposes the REST API.
type Server struct {
	store        broadcast.Store
	engine       *webrtc.Engine
	policy       *delivery.DomainPolicy
	signer       *delivery.TokenSigner
	log          *slog.Logger
	webhooks     *webhook.Dispatcher
	auth         config.AuthConfig
	startedAt    time.Time
	transcodes   func() int
	hlsBase      string
	dashBase     string
	cluster      *cluster.Manager
	recordings   storage.Store
	recordActive func() int
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
		store:        opts.Store,
		engine:       opts.Engine,
		policy:       opts.Policy,
		signer:       opts.Signer,
		log:          log,
		webhooks:     opts.Webhooks,
		auth:         opts.Auth,
		startedAt:    startedAt,
		transcodes:   opts.Transcodes,
		hlsBase:      opts.HLSBase,
		dashBase:     opts.DashBase,
		cluster:      opts.Cluster,
		recordings:   opts.Recordings,
		recordActive: opts.RecordActive,
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
	mux.Handle("GET /api/v1/cluster/nodes", protect(s.handleClusterNodes))
	mux.Handle("GET /api/v1/recordings", protect(s.handleListRecordings))
	mux.Handle("DELETE /api/v1/recordings/{key...}", protect(s.handleDeleteRecording))

	mux.Handle("GET /api/v1/streams", protect(s.handleListStreams))
	mux.Handle("POST /api/v1/streams", protect(s.handleCreateStream))
	mux.Handle("GET /api/v1/streams/{id}", protect(s.handleGetStream))
	mux.Handle("DELETE /api/v1/streams/{id}", protect(s.handleDeleteStream))
	mux.Handle("GET /api/v1/streams/{id}/stats", protect(s.handleStreamStats))
	mux.Handle("PUT /api/v1/streams/{id}/delivery", protect(s.handleSetDelivery))
	mux.Handle("POST /api/v1/streams/{id}/token", protect(s.handleCreateToken))

	mux.Handle("GET /api/v1/security/domains", protect(s.handleGetGlobalDomains))
	mux.Handle("PUT /api/v1/security/domains", protect(s.handlePutGlobalDomains))
	mux.Handle("GET /api/v1/security/streams/{id}/domains", protect(s.handleGetStreamDomains))
	mux.Handle("PUT /api/v1/security/streams/{id}/domains", protect(s.handlePutStreamDomains))
	mux.Handle("GET /api/v1/security/token", protect(s.handleGetTokenConfig))
	mux.Handle("PUT /api/v1/security/token", protect(s.handlePutTokenConfig))
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
	transcoding := 0
	if s.transcodes != nil {
		transcoding = s.transcodes()
	}
	payload := map[string]any{
		"uptimeSeconds": int(time.Since(s.startedAt).Seconds()),
		"startedAt":     s.startedAt,
		"streamsTotal":  len(streams),
		"streamsLive":   live,
		"viewers":       viewers,
		"engineStreams": s.engine.StreamCount(),
		"transcoding":   transcoding,
	}
	if s.cluster != nil {
		payload["nodeId"] = s.cluster.NodeID()
		if nodes, err := s.cluster.Nodes(rctx()); err == nil {
			payload["nodes"] = len(nodes)
		}
	}
	if s.recordActive != nil {
		payload["recording"] = s.recordActive()
	}
	writeJSON(w, http.StatusOK, payload)
}

// handleListRecordings lists stored recordings.
func (s *Server) handleListRecordings(w http.ResponseWriter, _ *http.Request) {
	if s.recordings == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "count": 0, "recordings": []any{}})
		return
	}
	objs, err := s.recordings.List(rctx(), "")
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "storage_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "count": len(objs), "recordings": objs})
}

// handleDeleteRecording removes a recording by storage key.
func (s *Server) handleDeleteRecording(w http.ResponseWriter, r *http.Request) {
	if s.recordings == nil {
		writeError(w, http.StatusServiceUnavailable, "storage_unavailable", "recording storage is not configured")
		return
	}
	key := r.PathValue("key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "invalid_key", "recording key is required")
		return
	}
	if err := s.recordings.Delete(rctx(), key); err != nil {
		writeError(w, http.StatusInternalServerError, "storage_error", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleClusterNodes returns the current cluster membership.
func (s *Server) handleClusterNodes(w http.ResponseWriter, _ *http.Request) {
	if s.cluster == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	nodes, err := s.cluster.Nodes(rctx())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "cluster_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": true,
		"nodeId":  s.cluster.NodeID(),
		"count":   len(nodes),
		"nodes":   nodes,
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
	Delivery string            `json:"delivery"`
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
	if req.Delivery != "" {
		mode := broadcast.Delivery(strings.ToLower(strings.TrimSpace(req.Delivery)))
		if !mode.Valid() {
			writeError(w, http.StatusBadRequest, "invalid_delivery", "delivery must be one of all, webrtc, hls, dash")
			return
		}
		b.SetDelivery(mode)
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
	b, err := s.lookupStream(r.PathValue("id"))
	if err != nil {
		s.writeLookupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.snapshot(b))
}

func (s *Server) handleDeleteStream(w http.ResponseWriter, r *http.Request) {
	b, err := s.lookupStream(r.PathValue("id"))
	if err != nil {
		s.writeLookupError(w, err)
		return
	}
	if err := s.store.Delete(rctx(), b.ID()); err != nil {
		s.writeLookupError(w, err)
		return
	}
	s.policy.DeleteStream(b.StreamID())
	s.emit(webhook.EventStreamDeleted, b.StreamID(), nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStreamStats(w http.ResponseWriter, r *http.Request) {
	b, err := s.lookupStream(r.PathValue("id"))
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

type deliveryRequest struct {
	Delivery string `json:"delivery"`
}

func (s *Server) handleSetDelivery(w http.ResponseWriter, r *http.Request) {
	b, err := s.lookupStream(r.PathValue("id"))
	if err != nil {
		s.writeLookupError(w, err)
		return
	}
	var req deliveryRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	mode := broadcast.Delivery(strings.ToLower(strings.TrimSpace(req.Delivery)))
	if !mode.Valid() {
		writeError(w, http.StatusBadRequest, "invalid_delivery", "delivery must be one of all, webrtc, hls, dash")
		return
	}
	b.SetDelivery(mode)
	writeJSON(w, http.StatusOK, s.snapshot(b))
}

// handleCreateToken returns a signed playback token (when tokenization is on)
// and the tokenized HLS/DASH URLs for the stream's delivery mode.
func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	b, err := s.lookupStream(r.PathValue("id"))
	if err != nil {
		s.writeLookupError(w, err)
		return
	}
	mode := b.Delivery()
	streamID := b.StreamID()

	resp := map[string]any{
		"streamId":     streamID,
		"delivery":     string(mode),
		"tokenEnabled": s.signer != nil && s.signer.Enabled(),
	}

	var token string
	if s.signer != nil && s.signer.Enabled() {
		tok, exp := s.signer.Sign(streamID)
		token = tok
		resp["token"] = tok
		resp["expiresAt"] = exp
	}
	if mode.UsesHLS() {
		resp["hlsUrl"] = s.mediaURL(s.hlsBase, streamID, "index.m3u8", token)
	}
	if mode.UsesDASH() {
		resp["dashUrl"] = s.mediaURL(s.dashBase, streamID, "manifest.mpd", token)
	}
	writeJSON(w, http.StatusOK, resp)
}

type tokenConfigRequest struct {
	Enabled    *bool   `json:"enabled"`
	TTLSeconds *int    `json:"ttlSeconds"`
	Secret     *string `json:"secret"`
}

func (s *Server) handleGetTokenConfig(w http.ResponseWriter, _ *http.Request) {
	ttl := 0
	if s.signer != nil {
		ttl = int(s.signer.TTL().Seconds())
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":    s.signer != nil && s.signer.Enabled(),
		"ttlSeconds": ttl,
	})
}

func (s *Server) handlePutTokenConfig(w http.ResponseWriter, r *http.Request) {
	if s.signer == nil {
		writeError(w, http.StatusServiceUnavailable, "token_unavailable", "token signing is not configured")
		return
	}
	var req tokenConfigRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if req.Secret != nil && *req.Secret != "" {
		s.signer.SetSecret(*req.Secret)
	}
	if req.TTLSeconds != nil && *req.TTLSeconds > 0 {
		s.signer.SetTTL(time.Duration(*req.TTLSeconds) * time.Second)
	}
	if req.Enabled != nil {
		if *req.Enabled {
			s.signer.EnsureSecret()
		}
		s.signer.SetEnabled(*req.Enabled)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":    s.signer.Enabled(),
		"ttlSeconds": int(s.signer.TTL().Seconds()),
	})
}

// mediaURL builds a playback URL, inserting a token path segment when present.
func (s *Server) mediaURL(base, streamID, file, token string) string {
	segment := streamID
	if token != "" {
		segment = token + "/" + streamID
	}
	return "/" + strings.Trim(strings.Join([]string{strings.Trim(base, "/"), segment, file}, "/"), "/")
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

// lookupStream resolves a broadcast by id, falling back to the stream key so
// callers may use either identifier.
func (s *Server) lookupStream(idOrStream string) (*broadcast.Broadcast, error) {
	if b, err := s.store.Get(rctx(), idOrStream); err == nil {
		return b, nil
	}
	return s.store.GetByStreamID(rctx(), idOrStream)
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
