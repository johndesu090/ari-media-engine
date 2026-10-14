// Package config defines the Ari Media Engine configuration schema, default
// values and environment-variable overrides.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration document.
type Config struct {
	Server    ServerConfig    `yaml:"server"`
	WebRTC    WebRTCConfig    `yaml:"webrtc"`
	Log       LogConfig       `yaml:"log"`
	Storage   StorageConfig   `yaml:"storage"`
	Cluster   ClusterConfig   `yaml:"cluster"`
	Ingest    IngestConfig    `yaml:"ingest"`
	HLS       HLSConfig       `yaml:"hls"`
	Auth      AuthConfig      `yaml:"auth"`
	Security  SecurityConfig  `yaml:"security"`
	Player    PlayerConfig    `yaml:"player"`
	Webhooks  WebhookConfig   `yaml:"webhooks"`
	Transcode TranscodeConfig `yaml:"transcode"`
	Token     TokenConfig     `yaml:"token"`
	Recording RecordingConfig `yaml:"recording"`
	P2P       P2PConfig       `yaml:"p2p"`
	Audio     AudioConfig     `yaml:"audio"`
}

// AudioConfig controls AAC→Opus transcoding so WebRTC subscribers receive audio.
type AudioConfig struct {
	Enabled bool   `yaml:"enabled"`
	Bitrate string `yaml:"bitrate"`
}

// P2PConfig controls the peer-assisted delivery tracker.
type P2PConfig struct {
	Enabled  bool   `yaml:"enabled"`
	WSPath   string `yaml:"ws_path"`
	MaxPeers int    `yaml:"max_peers"`
}

// ServerConfig controls the HTTP management/API server.
type ServerConfig struct {
	Host               string `yaml:"host"`
	Port               int    `yaml:"port"`
	ReadTimeoutSec     int    `yaml:"read_timeout_sec"`
	WriteTimeoutSec    int    `yaml:"write_timeout_sec"`
	ShutdownTimeoutSec int    `yaml:"shutdown_timeout_sec"`
	DashboardDir       string `yaml:"dashboard_dir"`
	PublicURL          string `yaml:"public_url"`
}

// WebRTCConfig controls the Pion WebRTC transport.
type WebRTCConfig struct {
	ICEPublicIPs   []string `yaml:"ice_public_ips"`
	ICETCPPort     int      `yaml:"ice_tcp_port"`
	ICEUDPPortMin  int      `yaml:"ice_udp_port_min"`
	ICEUDPPortMax  int      `yaml:"ice_udp_port_max"`
	EnableLoopback bool     `yaml:"enable_loopback"`
	MaxSubscribers int      `yaml:"max_subscribers_per_broadcast"`
}

// LogConfig controls structured logging.
type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// StorageConfig selects the recording/object storage backend.
type StorageConfig struct {
	Driver      string `yaml:"driver"`
	LocalRoot   string `yaml:"local_root"`
	PublicBase  string `yaml:"public_base"`
	S3Endpoint  string `yaml:"s3_endpoint"`
	S3Bucket    string `yaml:"s3_bucket"`
	S3Region    string `yaml:"s3_region"`
	S3AccessKey string `yaml:"s3_access_key"`
	S3SecretKey string `yaml:"s3_secret_key"`
	S3UseSSL    bool   `yaml:"s3_use_ssl"`
}

// RecordingConfig controls stream recording.
type RecordingConfig struct {
	Enabled    bool   `yaml:"enabled"`
	AutoRecord bool   `yaml:"auto_record"`
	WorkDir    string `yaml:"work_dir"`
}

// ClusterConfig controls horizontal scaling state.
type ClusterConfig struct {
	Enabled      bool   `yaml:"enabled"`
	RedisURL     string `yaml:"redis_url"`
	NodeID       string `yaml:"node_id"`
	HeartbeatSec int    `yaml:"heartbeat_sec"`
}

// IngestConfig holds the non-WebRTC ingest protocols.
type IngestConfig struct {
	RTMP IngestProtocol `yaml:"rtmp"`
	SRT  IngestProtocol `yaml:"srt"`
	RTSP IngestProtocol `yaml:"rtsp"`
}

// IngestProtocol toggles a single ingest listener.
type IngestProtocol struct {
	Enabled bool `yaml:"enabled"`
	Port    int  `yaml:"port"`
	// Sources are RTSP pull URLs to start at boot ("url" or "streamId=url").
	Sources []string `yaml:"sources"`
}

// HLSConfig controls HLS/DASH packaging.
type HLSConfig struct {
	Enabled      bool   `yaml:"enabled"`
	SegmentSec   int    `yaml:"segment_sec"`
	PlaylistSize int    `yaml:"playlist_size"`
	LowLatency   bool   `yaml:"low_latency"`
	OutputDir    string `yaml:"output_dir"`
}

// AuthConfig controls API authentication and the dashboard login.
type AuthConfig struct {
	Enabled    bool   `yaml:"enabled"`
	JWTSecret  string `yaml:"jwt_secret"`
	APIKey     string `yaml:"api_key"`
	Username   string `yaml:"username"`
	Password   string `yaml:"password"`
	SessionTTL int    `yaml:"session_ttl_sec"`
}

// SecurityConfig controls iframe embedding restrictions. When AllowAllEmbed is
// false only the domains listed in AllowedDomains (globally or per stream) may
// frame a stream, enforced through the Content-Security-Policy frame-ancestors
// directive and the embed guard middleware.
type SecurityConfig struct {
	AllowAllEmbed  bool     `yaml:"allow_all_embed"`
	AllowedDomains []string `yaml:"allowed_domains"`
	EnforceOrigin  bool     `yaml:"enforce_origin"`
}

// PlayerConfig controls the built-in HTML5 player and its protocol fallbacks.
type PlayerConfig struct {
	Title      string `yaml:"title"`
	WSPath     string `yaml:"ws_path"`
	HLSBase    string `yaml:"hls_base"`
	DashBase   string `yaml:"dash_base"`
	LowLatency bool   `yaml:"low_latency_default"`
	// DashJSURL optionally loads a dash.js build for DASH playback.
	DashJSURL string `yaml:"dash_js_url"`
	// HLSJSURL optionally loads an hls.js build for HLS playback.
	HLSJSURL string `yaml:"hls_js_url"`
}

// TokenConfig controls signed playback tokens for HLS/DASH.
type TokenConfig struct {
	Enabled bool   `yaml:"enabled"`
	Secret  string `yaml:"secret"`
	TTL     int    `yaml:"ttl_sec"`
}

// WebhookConfig controls stream lifecycle webhook delivery.
type WebhookConfig struct {
	Enabled bool     `yaml:"enabled"`
	URLs    []string `yaml:"urls"`
	Secret  string   `yaml:"secret"`
}

// TranscodeConfig controls the FFmpeg ABR ladder that produces HLS renditions
// for each ingested stream.
type TranscodeConfig struct {
	Enabled    bool              `yaml:"enabled"`
	FFmpegPath string            `yaml:"ffmpeg_path"`
	OutputDir  string            `yaml:"output_dir"`
	SegmentSec int               `yaml:"segment_sec"`
	ListSize   int               `yaml:"list_size"`
	Preset     string            `yaml:"preset"`
	Renditions []RenditionConfig `yaml:"renditions"`
}

// RenditionConfig is a single rung of the ABR ladder.
type RenditionConfig struct {
	Name    string `yaml:"name"`
	Width   int    `yaml:"width"`
	Height  int    `yaml:"height"`
	Bitrate string `yaml:"bitrate"`
	MaxRate string `yaml:"maxrate"`
	BufSize string `yaml:"bufsize"`
}

// Default returns a fully-populated configuration with sane development values.
func Default() Config {
	return Config{
		Server: ServerConfig{
			Host:               "0.0.0.0",
			Port:               8080,
			ReadTimeoutSec:     15,
			WriteTimeoutSec:    15,
			ShutdownTimeoutSec: 15,
			DashboardDir:       "web/dashboard",
		},
		WebRTC: WebRTCConfig{
			ICETCPPort:     7881,
			ICEUDPPortMin:  50000,
			ICEUDPPortMax:  50100,
			MaxSubscribers: 1000,
		},
		Log: LogConfig{Level: "info", Format: "json"},
		Storage: StorageConfig{
			Driver:    "local",
			LocalRoot: "./data/recordings",
		},
		Cluster: ClusterConfig{Enabled: false, HeartbeatSec: 5},
		Ingest: IngestConfig{
			RTMP: IngestProtocol{Enabled: true, Port: 1935},
			SRT:  IngestProtocol{Enabled: true, Port: 8890},
			RTSP: IngestProtocol{Enabled: false, Port: 8554},
		},
		HLS: HLSConfig{
			Enabled:      true,
			SegmentSec:   2,
			PlaylistSize: 6,
			OutputDir:    "./data/hls",
		},
		Auth: AuthConfig{Enabled: false, SessionTTL: 43200},
		Security: SecurityConfig{
			AllowAllEmbed: false,
			EnforceOrigin: true,
		},
		Player: PlayerConfig{
			Title:      "Ari Media Engine",
			WSPath:     "/ws",
			HLSBase:    "/hls",
			DashBase:   "/dash",
			LowLatency: true,
		},
		Webhooks: WebhookConfig{Enabled: false},
		Token:    TokenConfig{Enabled: false, TTL: 3600},
		Recording: RecordingConfig{
			Enabled:    false,
			AutoRecord: true,
			WorkDir:    "./data/recordings",
		},
		P2P: P2PConfig{
			Enabled:  false,
			WSPath:   "/p2p/ws",
			MaxPeers: 4,
		},
		Audio: AudioConfig{Enabled: false, Bitrate: "96k"},
		Transcode: TranscodeConfig{
			Enabled:    false,
			FFmpegPath: "ffmpeg",
			OutputDir:  "./data/hls",
			SegmentSec: 2,
			ListSize:   6,
			Preset:     "veryfast",
			Renditions: []RenditionConfig{
				{Name: "1080p", Width: 1920, Height: 1080, Bitrate: "5000k", MaxRate: "5350k", BufSize: "7500k"},
				{Name: "720p", Width: 1280, Height: 720, Bitrate: "2800k", MaxRate: "2996k", BufSize: "4200k"},
				{Name: "480p", Width: 854, Height: 480, Bitrate: "1200k", MaxRate: "1284k", BufSize: "1800k"},
			},
		},
	}
}

// Load reads YAML from path (when non-empty), applies it over Default, then
// applies environment overrides and validates the result.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("config: read %q: %w", path, err)
		}
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("config: parse %q: %w", path, err)
		}
	}
	applyEnv(&cfg)
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func applyEnv(cfg *Config) {
	setString(&cfg.Server.Host, "ARI_SERVER_HOST")
	setInt(&cfg.Server.Port, "ARI_SERVER_PORT")
	setString(&cfg.Server.PublicURL, "ARI_SERVER_PUBLIC_URL")
	setString(&cfg.Server.DashboardDir, "ARI_DASHBOARD_DIR")
	setStringSlice(&cfg.WebRTC.ICEPublicIPs, "ARI_ICE_PUBLIC_IPS")
	setInt(&cfg.WebRTC.ICETCPPort, "ARI_ICE_TCP_PORT")
	setInt(&cfg.WebRTC.ICEUDPPortMin, "ARI_ICE_UDP_PORT_MIN")
	setInt(&cfg.WebRTC.ICEUDPPortMax, "ARI_ICE_UDP_PORT_MAX")
	setBool(&cfg.WebRTC.EnableLoopback, "ARI_WEBRTC_ENABLE_LOOPBACK")
	setBool(&cfg.Ingest.RTMP.Enabled, "ARI_RTMP_ENABLED")
	setInt(&cfg.Ingest.RTMP.Port, "ARI_RTMP_PORT")
	setBool(&cfg.Ingest.SRT.Enabled, "ARI_SRT_ENABLED")
	setInt(&cfg.Ingest.SRT.Port, "ARI_SRT_PORT")
	setBool(&cfg.Ingest.RTSP.Enabled, "ARI_RTSP_ENABLED")
	setInt(&cfg.Ingest.RTSP.Port, "ARI_RTSP_PORT")
	setBool(&cfg.HLS.Enabled, "ARI_HLS_ENABLED")
	setString(&cfg.HLS.OutputDir, "ARI_HLS_OUTPUT_DIR")
	setString(&cfg.Log.Level, "ARI_LOG_LEVEL")
	setString(&cfg.Log.Format, "ARI_LOG_FORMAT")
	setString(&cfg.Storage.Driver, "ARI_STORAGE_DRIVER")
	setString(&cfg.Storage.LocalRoot, "ARI_STORAGE_LOCAL_ROOT")
	setBool(&cfg.Cluster.Enabled, "ARI_CLUSTER_ENABLED")
	setString(&cfg.Cluster.RedisURL, "ARI_REDIS_URL")
	setString(&cfg.Cluster.NodeID, "ARI_NODE_ID")
	setBool(&cfg.Auth.Enabled, "ARI_AUTH_ENABLED")
	setString(&cfg.Auth.JWTSecret, "ARI_JWT_SECRET")
	setString(&cfg.Auth.APIKey, "ARI_API_KEY")
	setString(&cfg.Auth.Username, "ARI_AUTH_USERNAME")
	setString(&cfg.Auth.Password, "ARI_AUTH_PASSWORD")
	setInt(&cfg.Auth.SessionTTL, "ARI_AUTH_SESSION_TTL_SEC")
	setBool(&cfg.Security.AllowAllEmbed, "ARI_SECURITY_ALLOW_ALL_EMBED")
	setStringSlice(&cfg.Security.AllowedDomains, "ARI_SECURITY_ALLOWED_DOMAINS")
	setBool(&cfg.Security.EnforceOrigin, "ARI_SECURITY_ENFORCE_ORIGIN")
	setString(&cfg.Player.Title, "ARI_PLAYER_TITLE")
	setString(&cfg.Player.WSPath, "ARI_PLAYER_WS_PATH")
	setBool(&cfg.Webhooks.Enabled, "ARI_WEBHOOKS_ENABLED")
	setStringSlice(&cfg.Webhooks.URLs, "ARI_WEBHOOK_URLS")
	setString(&cfg.Webhooks.Secret, "ARI_WEBHOOK_SECRET")
	setBool(&cfg.Transcode.Enabled, "ARI_TRANSCODE_ENABLED")
	setString(&cfg.Transcode.FFmpegPath, "ARI_TRANSCODE_FFMPEG_PATH")
	setString(&cfg.Transcode.OutputDir, "ARI_TRANSCODE_OUTPUT_DIR")
	setInt(&cfg.Transcode.SegmentSec, "ARI_TRANSCODE_SEGMENT_SEC")
	setBool(&cfg.Token.Enabled, "ARI_TOKEN_ENABLED")
	setString(&cfg.Token.Secret, "ARI_TOKEN_SECRET")
	setInt(&cfg.Token.TTL, "ARI_TOKEN_TTL_SEC")
	setString(&cfg.Player.DashJSURL, "ARI_PLAYER_DASH_JS_URL")
	setBool(&cfg.Recording.Enabled, "ARI_RECORDING_ENABLED")
	setBool(&cfg.Recording.AutoRecord, "ARI_RECORDING_AUTO")
	setString(&cfg.Recording.WorkDir, "ARI_RECORDING_WORK_DIR")
	setString(&cfg.Storage.PublicBase, "ARI_STORAGE_PUBLIC_BASE")
	setBool(&cfg.Storage.S3UseSSL, "ARI_STORAGE_S3_USE_SSL")
	setBool(&cfg.P2P.Enabled, "ARI_P2P_ENABLED")
	setString(&cfg.P2P.WSPath, "ARI_P2P_WS_PATH")
	setInt(&cfg.P2P.MaxPeers, "ARI_P2P_MAX_PEERS")
	setString(&cfg.Player.HLSJSURL, "ARI_PLAYER_HLS_JS_URL")
	setBool(&cfg.Audio.Enabled, "ARI_AUDIO_ENABLED")
	setString(&cfg.Audio.Bitrate, "ARI_AUDIO_BITRATE")
	setStringSlice(&cfg.Ingest.RTSP.Sources, "ARI_RTSP_SOURCES")
}

func setStringSlice(dst *[]string, key string) {
	v, ok := os.LookupEnv(key)
	if !ok {
		return
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	*dst = out
}

func setString(dst *string, key string) {
	if v, ok := os.LookupEnv(key); ok {
		*dst = v
	}
}

func setInt(dst *int, key string) {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			*dst = n
		}
	}
}

func setBool(dst *bool, key string) {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			*dst = b
		}
	}
}

// Validate checks structural invariants of the configuration.
func (c Config) Validate() error {
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		return fmt.Errorf("config: server.port %d out of range", c.Server.Port)
	}
	if c.WebRTC.ICEUDPPortMin > c.WebRTC.ICEUDPPortMax {
		return fmt.Errorf("config: webrtc.ice_udp_port_min %d greater than max %d",
			c.WebRTC.ICEUDPPortMin, c.WebRTC.ICEUDPPortMax)
	}
	if c.Storage.Driver != "local" && c.Storage.Driver != "s3" {
		return fmt.Errorf("config: storage.driver %q unsupported (want local or s3)", c.Storage.Driver)
	}
	if c.Auth.Enabled && c.Auth.JWTSecret == "" && c.Auth.APIKey == "" && c.Auth.Username == "" {
		return fmt.Errorf("config: auth.enabled requires api_key, jwt_secret or username/password")
	}
	if c.Auth.Username != "" && c.Auth.Password == "" {
		return fmt.Errorf("config: auth.username requires auth.password")
	}
	if c.Player.WSPath == "" || !strings.HasPrefix(c.Player.WSPath, "/") {
		return fmt.Errorf("config: player.ws_path %q must start with /", c.Player.WSPath)
	}
	if c.Transcode.Enabled {
		if c.Transcode.FFmpegPath == "" {
			return fmt.Errorf("config: transcode.ffmpeg_path is required when transcode is enabled")
		}
		if len(c.Transcode.Renditions) == 0 {
			return fmt.Errorf("config: transcode.enabled requires at least one rendition")
		}
		if c.Transcode.SegmentSec <= 0 {
			return fmt.Errorf("config: transcode.segment_sec must be positive")
		}
	}
	if c.Token.Enabled && c.Token.Secret == "" {
		return fmt.Errorf("config: token.enabled requires token.secret")
	}
	if c.Cluster.Enabled && c.Cluster.RedisURL == "" {
		return fmt.Errorf("config: cluster.enabled requires cluster.redis_url")
	}
	if c.P2P.Enabled && c.P2P.WSPath == "" {
		return fmt.Errorf("config: p2p.ws_path is required when p2p is enabled")
	}
	return nil
}

// ReadTimeout returns the configured HTTP read timeout.
func (c Config) ReadTimeout() time.Duration {
	return time.Duration(c.Server.ReadTimeoutSec) * time.Second
}

// WriteTimeout returns the configured HTTP write timeout.
func (c Config) WriteTimeout() time.Duration {
	return time.Duration(c.Server.WriteTimeoutSec) * time.Second
}

// ShutdownTimeout returns the configured graceful-shutdown deadline.
func (c Config) ShutdownTimeout() time.Duration {
	return time.Duration(c.Server.ShutdownTimeoutSec) * time.Second
}

// Addr returns the host:port listen address for the HTTP server.
func (c Config) Addr() string {
	return fmt.Sprintf("%s:%d", c.Server.Host, c.Server.Port)
}
