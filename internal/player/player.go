// Package player implements the Ari Media Engine built-in HTML5 video player.
// It renders a self-hosted, embeddable page at /player.html?id=STREAM_ID and
// serves its CSS/JS assets. The page selects the best available playback
// protocol at runtime: WebRTC for sub-second latency, falling back to HLS or
// DASH when WebRTC is blocked by the network.
package player

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
)

//go:embed assets
var assets embed.FS

// AssetPath is the URL prefix under which player assets are served.
const AssetPath = "/player/assets"

// Config configures the player renderer.
type Config struct {
	// Title is shown in the page title and as the default UI label.
	Title string
	// WSPath is the signaling WebSocket path (e.g. /ws).
	WSPath string
	// HLSBase is the HLS output base path (e.g. /hls).
	HLSBase string
	// DashBase is the DASH output base path (e.g. /dash).
	DashBase string
	// PublicURL is the externally reachable base URL. When empty the player
	// uses same-origin relative URLs.
	PublicURL string
	// LowLatency is the default state of the low-latency toggle.
	LowLatency bool
}

// Handler renders the player page and serves its static assets.
type Handler struct {
	tmpl *template.Template
	cfg  Config
}

// New parses the embedded templates and returns a Handler.
func New(cfg Config) (*Handler, error) {
	tmpl, err := template.ParseFS(assets, "assets/*.html")
	if err != nil {
		return nil, fmt.Errorf("player: parse templates: %w", err)
	}
	return &Handler{tmpl: tmpl, cfg: cfg}, nil
}

// viewData is the template payload.
type viewData struct {
	StreamID   string
	Title      string
	AssetBase  string
	ConfigJSON template.JS
}

// playerConfig is serialized into the page for the JavaScript runtime.
type playerConfig struct {
	StreamID   string `json:"streamId"`
	WSPath     string `json:"wsPath"`
	HLSURL     string `json:"hlsUrl"`
	DashURL    string `json:"dashUrl"`
	LowLatency bool   `json:"lowLatency"`
}

// ServePlayer renders /player.html?id=STREAM_ID.
func (h *Handler) ServePlayer(w http.ResponseWriter, r *http.Request) {
	streamID := strings.TrimSpace(r.URL.Query().Get("id"))
	if streamID == "" {
		http.Error(w, "missing required query parameter: id", http.StatusBadRequest)
		return
	}

	cfg := playerConfig{
		StreamID:   streamID,
		WSPath:     h.cfg.WSPath,
		HLSURL:     joinURL(h.cfg.PublicURL, h.cfg.HLSBase, streamID, "index.m3u8"),
		DashURL:    joinURL(h.cfg.PublicURL, h.cfg.DashBase, streamID, "manifest.mpd"),
		LowLatency: h.cfg.LowLatency,
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data := viewData{
		StreamID:  streamID,
		Title:     h.cfg.Title,
		AssetBase: AssetPath,
		// Prevent a stream id containing "</script>" from breaking out.
		ConfigJSON: template.JS(strings.ReplaceAll(string(raw), "</", `<\/`)),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := h.tmpl.ExecuteTemplate(w, "player.html", data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// Assets serves the embedded CSS/JS bundle under AssetPath. The request path
// "/player/assets/x" is reduced to the embedded path "assets/x".
func (h *Handler) Assets() http.Handler {
	return http.StripPrefix("/player/", http.FileServer(http.FS(assets)))
}

// joinURL builds a playback URL from a base, path, stream id and filename.
func joinURL(publicURL, base, streamID, file string) string {
	path := strings.Trim(strings.Join([]string{strings.Trim(base, "/"), streamID, file}, "/"), "/")
	if publicURL == "" {
		return "/" + path
	}
	return strings.TrimRight(publicURL, "/") + "/" + path
}
