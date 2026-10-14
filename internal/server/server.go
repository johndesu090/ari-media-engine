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
	"github.com/johndesu090/ari-media-engine/internal/cluster"
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
	cluster     *cluster.Manager

	http *http.Server
}

// BuildVersion is the engine version advertised to the cluster; main overrides
// it at startup.
var BuildVersion = "dev"

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
	signer := delivery.NewTokenSigner(cfg.Token.Enabled, cfg.Token.Secret, time.Duration(cfg.Token.TTL)*time.Second)

	deliveryOf := func(streamID string) broadcast.Delivery {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if b, err := store.GetByStreamID(ctx, streamID); err == nil {
			return b.Delivery()
		}
		return broadcast.DeliveryAll
	}

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
		DashJSURL:  cfg.Player.DashJSURL,
		Signer:     signer,
		Delivery:   deliveryOf,
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
			formats := deliveryFormats(deliveryOf(streamKey))
			if formats == 0 {
				return nil, nil
			}
			session, err := transcoder.Start(streamKey, formats)
			if err != nil {
				return nil, err
			}
			if session == nil {
				return nil, nil
			}
			return session, nil
		}
	}

	transcodes := func() int {
		if transcoder == nil {
			return 0
		}
		return len(transcoder.Active())
	}

	// Cluster membership (optional).
	var clusterMgr *cluster.Manager
	if cfg.Cluster.Enabled {
		ttl := time.Duration(cfg.Cluster.HeartbeatSec*3) * time.Second
		registry, err := cluster.NewRedisRegistry(cfg.Cluster.RedisURL, ttl)
		if err != nil {
			return nil, fmt.Errorf("server: cluster registry: %w", err)
		}
		nodeID := cfg.Cluster.NodeID
		if nodeID == "" {
			nodeID = id.New("node")
		}
		address := cfg.Server.PublicURL
		if address == "" {
			address = cfg.Addr()
		}
		clusterMgr = cluster.NewManager(cluster.Options{
			Registry:  registry,
			NodeID:    nodeID,
			Address:   address,
			Version:   BuildVersion,
			Heartbeat: time.Duration(cfg.Cluster.HeartbeatSec) * time.Second,
			Logger:    log,
			Stats: func() (int, int) {
				viewers := 0
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				if list, err := store.List(ctx); err == nil {
					for _, b := range list {
						viewers += b.ViewerCount()
					}
				}
				return engine.StreamCount(), viewers
			},
		})
	}

	apiSrv := api.New(api.Options{
		Store:      store,
		Engine:     engine,
		Policy:     policy,
		Signer:     signer,
		Logger:     log,
		Webhooks:   webhooks,
		Auth:       cfg.Auth,
		StartedAt:  time.Now().UTC(),
		Transcodes: transcodes,
		HLSBase:    cfg.Player.HLSBase,
		DashBase:   cfg.Player.DashBase,
		Cluster:    clusterMgr,
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

	// HLS/DASH output, served from disk with optional token enforcement.
	if cfg.HLS.Enabled {
		playbackRoot := cfg.HLS.OutputDir
		if cfg.Transcode.Enabled && cfg.Transcode.OutputDir != "" {
			playbackRoot = cfg.Transcode.OutputDir
		}
		if err := os.MkdirAll(playbackRoot, 0o755); err == nil {
			root.Handle("GET /hls/", &delivery.PlaybackServer{Root: playbackRoot, Prefix: "/hls", Signer: signer, Log: log})
			root.Handle("GET /dash/", &delivery.PlaybackServer{Root: playbackRoot, Prefix: "/dash", Signer: signer, Log: log})
		} else {
			log.Warn("playback output dir unavailable", "dir", playbackRoot, "err", err)
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
		cluster:    clusterMgr,
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
	if s.cluster != nil {
		if err := s.cluster.Start(ctx); err != nil {
			return err
		}
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
	if s.cluster != nil {
		clusterCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		s.cluster.Close(clusterCtx)
		cancel()
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

// deliveryFormats maps a delivery mode to the packaging formats to produce.
func deliveryFormats(d broadcast.Delivery) transcode.Format {
	var f transcode.Format
	if d.UsesHLS() {
		f |= transcode.FormatHLS
	}
	if d.UsesDASH() {
		f |= transcode.FormatDASH
	}
	return f
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
