// Package server wires the Ari Media Engine subsystems together and runs the
// HTTP control plane, signaling endpoint, player, dashboard and ingest
// listeners for a single node.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/johndesu090/ari-media-engine/internal/api"
	"github.com/johndesu090/ari-media-engine/internal/broadcast"
	"github.com/johndesu090/ari-media-engine/internal/config"
	"github.com/johndesu090/ari-media-engine/internal/delivery"
	"github.com/johndesu090/ari-media-engine/internal/ingest"
	"github.com/johndesu090/ari-media-engine/internal/ingest/rtmp"
	"github.com/johndesu090/ari-media-engine/internal/player"
	"github.com/johndesu090/ari-media-engine/internal/signaling"
	"github.com/johndesu090/ari-media-engine/internal/transcode"
	"github.com/johndesu090/ari-media-engine/internal/webhook"
	"github.com/johndesu090/ari-media-engine/internal/webrtc"
	"github.com/johndesu090/ari-media-engine/pkg/id"
)

// Server owns the lifecycle of a single Ari Media Engine node.
type Server struct {
	cfg config.Config
	log *slog.Logger

	engine *webrtc.Engine
	store  broadcast.Store
	policy *delivery.DomainPolicy
	ingest *ingest.Manager

	transcoder  *transcode.Manager
	rtmpHandler ingest.Handler

	http *http.Server
}

// New builds a Server from configuration.
func New(cfg config.Config, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}

	engine, err := webrtc.NewEngine(webrtc.Config{
		ICEPublicIPs:   cfg.WebRTC.ICEPublicIPs,
		ICETCPPort:     cfg.WebRTC.ICETCPPort,
		ICEUDPPortMin:  cfg.WebRTC.ICEUDPPortMin,
		ICEUDPPortMax:  cfg.WebRTC.ICEUDPPortMax,
		EnableLoopback: cfg.WebRTC.EnableLoopback,
		MaxSubscribers: cfg.WebRTC.MaxSubscribers,
	}, log)
	if err != nil {
		return nil, fmt.Errorf("server: webrtc engine: %w", err)
	}

	store := broadcast.NewMemoryStore()
	policy := delivery.NewDomainPolicy(cfg.Security.AllowedDomains, cfg.Security.AllowAllEmbed)

	var hooks signaling.Hooks
	var webhooks *webhook.Dispatcher
	if cfg.Webhooks.Enabled {
		webhooks = webhook.New(cfg.Webhooks.URLs, cfg.Webhooks.Secret, log)
	}
	hooks = signaling.Hooks{
		OnPublish: func(streamID string) {
			if webhooks != nil {
				webhooks.Emit(webhook.Event{Type: webhook.EventStreamPublished, StreamID: streamID})
			}
		},
		OnEnded: func(streamID string) {
			if webhooks != nil {
				webhooks.Emit(webhook.Event{Type: webhook.EventStreamEnded, StreamID: streamID})
			}
		},
	}

	sigSrv := signaling.New(signaling.Options{
		Engine: engine,
		Store:  store,
		Logger: log,
		Hooks:  hooks,
	})

	playerHandler, err := player.New(player.Config{
		Title:      cfg.Player.Title,
		WSPath:     cfg.Player.WSPath,
		HLSBase:    cfg.Player.HLSBase,
		DashBase:   cfg.Player.DashBase,
		PublicURL:  cfg.Server.PublicURL,
		LowLatency: cfg.Player.LowLatency,
	})
	if err != nil {
		return nil, fmt.Errorf("server: player: %w", err)
	}

	// ABR transcoding pipeline (optional).
	var transcoder *transcode.Manager
	var newFLVSink func(streamKey string) (rtmp.FLVSink, error)
	if cfg.Transcode.Enabled {
		transcoder = transcode.New(transcode.Config{
			FFmpegPath: cfg.Transcode.FFmpegPath,
			OutputDir:  cfg.Transcode.OutputDir,
			SegmentSec: cfg.Transcode.SegmentSec,
			ListSize:   cfg.Transcode.ListSize,
			Preset:     cfg.Transcode.Preset,
			Renditions: toRenditions(cfg.Transcode.Renditions),
		}, log)
		newFLVSink = func(streamKey string) (rtmp.FLVSink, error) {
			return transcoder.Start(streamKey)
		}
	}

	transcodes := func() int {
		if transcoder == nil {
			return 0
		}
		return len(transcoder.Active())
	}

	apiSrv := api.New(api.Options{
		Store:      store,
		Engine:     engine,
		Policy:     policy,
		Logger:     log,
		Webhooks:   webhooks,
		Auth:       cfg.Auth,
		StartedAt:  time.Now().UTC(),
		Transcodes: transcodes,
	})

	root := http.NewServeMux()
	apiSrv.Register(root)

	// WebSocket signaling.
	root.HandleFunc("GET "+cfg.Player.WSPath, sigSrv.HandleWS)

	// Embeddable player, guarded by the iframe domain policy.
	guard := delivery.EmbedGuard{
		Policy:   policy,
		StreamID: delivery.QueryStreamID("id"),
		Enforce:  cfg.Security.EnforceOrigin,
	}
	root.Handle("GET /player.html", guard.Middleware(http.HandlerFunc(playerHandler.ServePlayer)))
	root.Handle("GET "+player.AssetPath+"/", playerHandler.Assets())

	// HLS/DASH output, served from disk when packaging is enabled.
	if cfg.HLS.Enabled {
		hlsDir := cfg.HLS.OutputDir
		if err := os.MkdirAll(hlsDir, 0o755); err == nil {
			root.Handle("GET /hls/", http.StripPrefix("/hls/", http.FileServer(http.Dir(hlsDir))))
		} else {
			log.Warn("hls output dir unavailable", "dir", hlsDir, "err", err)
		}
	}

	// Management dashboard (static assets).
	registerDashboard(root, cfg.Server.DashboardDir)

	handler := delivery.SecurityHeaders()(root)

	return &Server{
		cfg:        cfg,
		log:        log,
		engine:     engine,
		store:      store,
		policy:     policy,
		ingest:     ingest.New(log),
		transcoder: transcoder,
		rtmpHandler: ingest.NewRTMPHandler(ingest.RTMPOptions{
			Engine: engine,
			Logger: log,
			Hooks: ingest.RTMPHooks{
				OnPublish:   func(streamKey string) { upsertStream(store, streamKey, broadcast.StatusLive) },
				OnUnpublish: func(streamKey string) { upsertStream(store, streamKey, broadcast.StatusEnded) },
			},
			NewFLVSink: newFLVSink,
		}),
		http: &http.Server{
			Addr:         cfg.Addr(),
			Handler:      handler,
			ReadTimeout:  cfg.ReadTimeout(),
			WriteTimeout: cfg.WriteTimeout(),
		},
	}, nil
}

// Addr returns the HTTP listen address.
func (s *Server) Addr() string {
	return s.cfg.Addr()
}

// Run starts every listener and blocks until ctx is cancelled, then shuts down
// gracefully.
func (s *Server) Run(ctx context.Context) error {
	if err := s.startIngest(ctx); err != nil {
		return err
	}

	errc := make(chan error, 1)
	go func() {
		s.log.Info("http server listening",
			"addr", s.Addr(),
			"player", "/player.html?id=STREAM_ID",
			"signaling", s.cfg.Player.WSPath,
		)
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		s.shutdown()
		return err
	case <-ctx.Done():
		s.log.Info("shutdown requested")
		s.shutdown()
		return nil
	}
}

func (s *Server) startIngest(ctx context.Context) error {
	var specs []ingest.Listener
	if s.cfg.Ingest.RTMP.Enabled {
		specs = append(specs, ingest.Listener{Protocol: ingest.ProtocolRTMP, Addr: fmt.Sprintf(":%d", s.cfg.Ingest.RTMP.Port), Handler: s.rtmpHandler})
	}
	if s.cfg.Ingest.SRT.Enabled {
		specs = append(specs, ingest.Listener{Protocol: ingest.ProtocolSRT, Addr: fmt.Sprintf(":%d", s.cfg.Ingest.SRT.Port)})
	}
	if s.cfg.Ingest.RTSP.Enabled {
		specs = append(specs, ingest.Listener{Protocol: ingest.ProtocolRTSP, Addr: fmt.Sprintf(":%d", s.cfg.Ingest.RTSP.Port)})
	}
	if len(specs) == 0 {
		return nil
	}
	return s.ingest.Start(ctx, specs)
}

func (s *Server) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout())
	defer cancel()

	if err := s.http.Shutdown(ctx); err != nil {
		s.log.Warn("http shutdown", "err", err)
	}
	if err := s.ingest.Close(); err != nil {
		s.log.Warn("ingest shutdown", "err", err)
	}
	if s.transcoder != nil {
		s.transcoder.StopAll()
	}
	s.engine.Shutdown()
}

// registerDashboard serves the management console from dir, falling back to a
// short informational page when the directory is absent.
func registerDashboard(mux *http.ServeMux, dir string) {
	index := filepath.Join(dir, "index.html")
	if _, err := os.Stat(index); err != nil {
		mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintln(w, "Ari Media Engine is running. Dashboard assets not found; see /healthz and /api/v1/stats.")
		})
		return
	}
	mux.Handle("GET /", http.FileServer(http.Dir(dir)))
}

// upsertStream creates or updates the control-plane record for an ingest stream
// so that RTMP (and future SRT/RTSP) publishers appear in the API and dashboard.
func upsertStream(store broadcast.Store, streamKey string, status broadcast.Status) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	b, err := store.GetByStreamID(ctx, streamKey)
	switch {
	case errors.Is(err, broadcast.ErrNotFound):
		b = broadcast.New(id.New("bc"), streamKey, streamKey, broadcast.TypeRTMP)
		if err := store.Create(ctx, b); err != nil {
			return
		}
	case err != nil:
		return
	}
	b.SetStatus(status)
}

// toRenditions converts configuration rungs into transcode renditions.
func toRenditions(in []config.RenditionConfig) []transcode.Rendition {
	out := make([]transcode.Rendition, 0, len(in))
	for _, r := range in {
		out = append(out, transcode.Rendition{
			Name:    r.Name,
			Width:   r.Width,
			Height:  r.Height,
			Bitrate: r.Bitrate,
			MaxRate: r.MaxRate,
			BufSize: r.BufSize,
		})
	}
	return out
}
