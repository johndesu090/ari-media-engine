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
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	pion "github.com/pion/webrtc/v4"

	"github.com/johndesu090/ari-media-engine/internal/api"
	"github.com/johndesu090/ari-media-engine/internal/audio"
	"github.com/johndesu090/ari-media-engine/internal/auth"
	"github.com/johndesu090/ari-media-engine/internal/broadcast"
	"github.com/johndesu090/ari-media-engine/internal/cluster"
	"github.com/johndesu090/ari-media-engine/internal/config"
	"github.com/johndesu090/ari-media-engine/internal/delivery"
	"github.com/johndesu090/ari-media-engine/internal/ingest"
	"github.com/johndesu090/ari-media-engine/internal/ingest/rtmp"
	"github.com/johndesu090/ari-media-engine/internal/ingest/rtsp"
	"github.com/johndesu090/ari-media-engine/internal/ingest/srt"
	"github.com/johndesu090/ari-media-engine/internal/p2p"
	"github.com/johndesu090/ari-media-engine/internal/player"
	"github.com/johndesu090/ari-media-engine/internal/recorder"
	"github.com/johndesu090/ari-media-engine/internal/signaling"
	"github.com/johndesu090/ari-media-engine/internal/storage"
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
	recorder    *recorder.Manager
	objectStore storage.Store
	audio       *audio.Manager
	rtsp        *rtsp.Manager
	srt         *srt.Manager

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
		ICEServers:     toICEServers(cfg.WebRTC.ICEServers),
		ICETCPOnly:     cfg.WebRTC.ICETransport == "tcp",
	}, log)
	if err != nil {
		return nil, fmt.Errorf("server: webrtc engine: %w", err)
	}

	store := broadcast.NewMemoryStore()
	policy := delivery.NewDomainPolicy(cfg.Security.AllowedDomains, cfg.Security.AllowAllEmbed)
	signer := delivery.NewTokenSigner(cfg.Token.Enabled, cfg.Token.Secret, time.Duration(cfg.Token.TTL)*time.Second)
	sessions := auth.NewStore(time.Duration(cfg.Auth.SessionTTL) * time.Second)

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
		HLSBase:    cfg.Player.HLSBase,
		DashBase:   cfg.Player.DashBase,
		PublicURL:  cfg.Server.PublicURL,
		LowLatency: cfg.Player.LowLatency,
		DashJSURL:  cfg.Player.DashJSURL,
		HLSJSURL:   cfg.Player.HLSJSURL,
		P2PEnabled: cfg.P2P.Enabled,
		P2PWsPath:  cfg.P2P.WSPath,
		Signer:     signer,
		Delivery:   deliveryOf,
	})
	if err != nil {
		return nil, fmt.Errorf("server: player: %w", err)
	}

	// Object storage for recordings.
	objectStore, err := storage.New(storage.Config{
		Driver:      cfg.Storage.Driver,
		LocalRoot:   cfg.Storage.LocalRoot,
		PublicBase:  cfg.Storage.PublicBase,
		S3Endpoint:  cfg.Storage.S3Endpoint,
		S3Bucket:    cfg.Storage.S3Bucket,
		S3Region:    cfg.Storage.S3Region,
		S3AccessKey: cfg.Storage.S3AccessKey,
		S3SecretKey: cfg.Storage.S3SecretKey,
		S3UseSSL:    cfg.Storage.S3UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("server: storage: %w", err)
	}

	// ABR transcoding pipeline (optional).
	var transcoder *transcode.Manager
	if cfg.Transcode.Enabled {
		transcoder = transcode.New(transcode.Config{
			FFmpegPath: cfg.Transcode.FFmpegPath,
			OutputDir:  cfg.Transcode.OutputDir,
			SegmentSec: cfg.Transcode.SegmentSec,
			ListSize:   cfg.Transcode.ListSize,
			Preset:     cfg.Transcode.Preset,
			Abr:        cfg.Transcode.Abr,
			Renditions: toRenditions(cfg.Transcode.Renditions),
		}, log)
	}

	// Recording pipeline (optional).
	var rec *recorder.Manager
	if cfg.Recording.Enabled {
		rec = recorder.New(recorder.Options{
			Store:   objectStore,
			WorkDir: cfg.Recording.WorkDir,
			Logger:  log,
		})
	}

	// AAC→Opus audio transcode for WebRTC subscribers (optional).
	var audioMgr *audio.Manager
	if cfg.Audio.Enabled {
		audioMgr = audio.NewManager(audio.Options{
			FFmpegPath: cfg.Transcode.FFmpegPath,
			Bitrate:    cfg.Audio.Bitrate,
			Logger:     log,
		})
	}

	// The FLV fan-out sink feeds transcoding and/or recording.
	var newFLVSink func(streamKey string) (rtmp.FLVSink, error)
	var newAudioSink func(streamKey string, session *webrtc.IngestSession) (rtmp.FLVSink, error)
	if audioMgr != nil {
		newAudioSink = func(streamKey string, session *webrtc.IngestSession) (rtmp.FLVSink, error) {
			s, err := audioMgr.Start(streamKey, session)
			if err != nil {
				return nil, err
			}
			if s == nil {
				return nil, nil
			}
			return s, nil
		}
	}
	if transcoder != nil || rec != nil {
		newFLVSink = func(streamKey string) (rtmp.FLVSink, error) {
			var sinks []rtmp.FLVSink
			if transcoder != nil {
				if formats := deliveryFormats(deliveryOf(streamKey)); formats != 0 {
					if s, err := transcoder.Start(streamKey, formats); err != nil {
						log.Warn("transcode sink unavailable", "stream", streamKey, "err", err)
					} else if s != nil {
						sinks = append(sinks, s)
					}
				}
			}
			if rec != nil && cfg.Recording.AutoRecord {
				if s, err := rec.Start(streamKey); err != nil {
					log.Warn("recorder sink unavailable", "stream", streamKey, "err", err)
				} else if s != nil {
					sinks = append(sinks, s)
				}
			}
			if len(sinks) == 0 {
				return nil, nil
			}
			return rtmp.MultiFLVSink(sinks...), nil
		}
	}

	transcodes := func() int {
		if transcoder == nil {
			return 0
		}
		return len(transcoder.Active())
	}
	recordings := func() int {
		if rec == nil {
			return 0
		}
		return len(rec.Active())
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

	// Peer-assisted delivery tracker (always available; toggled at runtime).
	p2pHub := p2p.NewHub(log, nil, cfg.P2P.MaxPeers)
	p2pHub.SetEnabled(cfg.P2P.Enabled)

	// RTSP pull manager (sources from config and the API).
	rtspMgr := rtsp.NewManager(engine, log, rtsp.Hooks{
		OnPublish:   func(id string) { upsertStream(store, id, broadcast.StatusLive) },
		OnUnpublish: func(id string) { upsertStream(store, id, broadcast.StatusEnded) },
	})

	// SRT ingest listener (optional).
	var srtMgr *srt.Manager
	if cfg.Ingest.SRT.Enabled {
		srtMgr = srt.NewManager(engine, fmt.Sprintf(":%d", cfg.Ingest.SRT.Port), log, srt.Hooks{
			OnPublish:   func(id string) { upsertStream(store, id, broadcast.StatusLive) },
			OnUnpublish: func(id string) { upsertStream(store, id, broadcast.StatusEnded) },
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
		Sessions:   sessions,
		StartedAt:  time.Now().UTC(),
		Transcodes: transcodes,
		TranscodeAbr: func() bool {
			return transcoder != nil && transcoder.Abr()
		},
		SetTranscodeAbr: func(v bool) error {
			if transcoder == nil {
				return fmt.Errorf("transcoding is not configured")
			}
			transcoder.SetAbr(v)
			return nil
		},
		Recordings: objectStore,
		RecordActive: func() int {
			return recordings()
		},
		HLSBase:  cfg.Player.HLSBase,
		DashBase: cfg.Player.DashBase,
		Cluster:  clusterMgr,
		P2P:      p2pHub,
		RTSP:     rtspMgr,
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
	root.Handle("GET "+player.TokenPath, guard.Middleware(http.HandlerFunc(playerHandler.ServeToken)))
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

	// Recordings served from local storage (local driver only).
	if cfg.Recording.Enabled && cfg.Storage.Driver == "local" && cfg.Storage.LocalRoot != "" {
		root.Handle("GET /recordings/", http.StripPrefix("/recordings/", http.FileServer(http.Dir(cfg.Storage.LocalRoot))))
	}

	// Peer-assisted delivery tracker and engine script.
	p2pWSPath := cfg.P2P.WSPath
	if p2pWSPath == "" {
		p2pWSPath = "/p2p/ws"
	}
	root.HandleFunc("GET "+p2pWSPath, p2pHub.HandleWS)
	root.Handle("GET /p2p/", http.StripPrefix("/p2p/", http.FileServer(http.Dir(filepath.Join("web", "p2p")))))

	// Management dashboard (static assets), gated by the dashboard login.
	root.Handle("GET /", dashboardGuard(cfg.Auth, sessions, dashboardHandler(cfg.Server.DashboardDir)))

	handler := delivery.SecurityHeaders(!cfg.Security.CSPPlayerOnly)(root)

	return &Server{
		cfg:         cfg,
		log:         log,
		engine:      engine,
		store:       store,
		policy:      policy,
		ingest:      ingest.New(log),
		transcoder:  transcoder,
		cluster:     clusterMgr,
		recorder:    rec,
		objectStore: objectStore,
		audio:       audioMgr,
		rtsp:        rtspMgr,
		srt:         srtMgr,
		rtmpHandler: ingest.NewRTMPHandler(ingest.RTMPOptions{
			Engine: engine,
			Logger: log,
			Hooks: ingest.RTMPHooks{
				OnPublish: func(streamKey string) { upsertStream(store, streamKey, broadcast.StatusLive) },
				OnUnpublish: func(streamKey string) {
					upsertStream(store, streamKey, broadcast.StatusEnded)
					if rec != nil {
						rec.Stop(streamKey)
					}
				},
			},
			NewFLVSink:   newFLVSink,
			NewAudioSink: newAudioSink,
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
	if s.srt != nil {
		if err := s.srt.Start(); err != nil {
			s.log.Warn("srt listener failed to start", "err", err)
		}
	}
	for _, src := range s.cfg.Ingest.RTSP.Sources {
		streamID, rawURL := parseRTSPSource(src)
		if streamID == "" {
			continue
		}
		if err := s.rtsp.Start(streamID, rawURL); err != nil {
			s.log.Warn("rtsp source start failed", "source", src, "err", err)
		}
	}

	errc := make(chan error, 1)
	go func() {
		scheme := "http"
		if s.cfg.Server.TLSCertFile != "" {
			scheme = "https"
		}
		s.log.Info("http server listening",
			"scheme", scheme,
			"addr", s.Addr(),
			"player", "/player.html?id=STREAM_ID",
			"signaling", s.cfg.Player.WSPath,
		)
		var err error
		if s.cfg.Server.TLSCertFile != "" && s.cfg.Server.TLSKeyFile != "" {
			err = s.http.ListenAndServeTLS(s.cfg.Server.TLSCertFile, s.cfg.Server.TLSKeyFile)
		} else {
			err = s.http.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
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
	if s.recorder != nil {
		s.recorder.StopAll()
	}
	if s.audio != nil {
		s.audio.StopAll()
	}
	if s.rtsp != nil {
		s.rtsp.StopAll()
	}
	if s.srt != nil {
		s.srt.Stop()
	}
	if s.objectStore != nil {
		_ = s.objectStore.Close()
	}
	if s.cluster != nil {
		clusterCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		s.cluster.Close(clusterCtx)
		cancel()
	}
	s.engine.Shutdown()
}

// dashboardGuard protects the dashboard pages: when authentication is enabled,
// requests without a valid session cookie are redirected to the login page. The
// login page itself is always served. Responses are marked no-store so a cached
// console can never bypass the gate.
func dashboardGuard(authCfg config.AuthConfig, sessions *auth.Store, next http.Handler) http.Handler {
	if !authCfg.Enabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		switch r.URL.Path {
		case "/login.html", "/login", "/login.js":
			next.ServeHTTP(w, r)
			return
		}
		if c, err := r.Cookie(auth.CookieName); err == nil && sessions.Valid(c.Value) {
			next.ServeHTTP(w, r)
			return
		}
		http.Redirect(w, r, "/login.html", http.StatusFound)
	})
}

// dashboardHandler serves the management console from dir, falling back to a
// short informational page when the directory is absent.
func dashboardHandler(dir string) http.Handler {
	index := filepath.Join(dir, "index.html")
	if _, err := os.Stat(index); err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintln(w, "Ari Media Engine is running. Dashboard assets not found; see /healthz and /api/v1/stats.")
		})
	}
	return http.FileServer(http.Dir(dir))
}

// toICEServers converts configuration ICE servers into Pion ICE servers.
func toICEServers(in []config.ICEServerConfig) []pion.ICEServer {
	out := make([]pion.ICEServer, 0, len(in))
	for _, s := range in {
		out = append(out, pion.ICEServer{URLs: s.URLs, Username: s.Username, Credential: s.Credential})
	}
	return out
}

// parseRTSPSource parses an RTSP source entry: "streamId=url" or a bare URL
// (whose stream id is derived from the last path segment).
func parseRTSPSource(entry string) (string, string) {
	entry = strings.TrimSpace(entry)
	if i := strings.IndexByte(entry, '='); i > 0 {
		return strings.TrimSpace(entry[:i]), strings.TrimSpace(entry[i+1:])
	}
	u, err := url.Parse(entry)
	if err != nil {
		return "", entry
	}
	name := path.Base(u.Path)
	if name == "" || name == "." || name == "/" {
		name = id.New("rtsp")
	}
	return name, entry
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
