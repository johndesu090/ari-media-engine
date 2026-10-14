// Package player implements the Ari Media Engine built-in HTML5 video player.
// It renders a self-hosted, embeddable page at /player.html?id=STREAM_ID and
// serves its CSS/JS assets. The page selects the best available playback
// protocol at runtime based on the stream's delivery mode: WebRTC for
// sub-second latency, falling back to HLS or DASH when WebRTC is blocked.
package player

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/johndesu090/ari-media-engine/internal/broadcast"
	"github.com/johndesu090/ari-media-engine/internal/delivery"
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
	// DashJSURL optionally loads a dash.js build for DASH playback.
	DashJSURL string
	// Signer mints playback tokens for HLS/DASH URLs when tokenization is on.
	Signer *delivery.TokenSigner
	// Delivery resolves the delivery mode for a stream id.
	Delivery func(streamID string) broadcast.Delivery
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
	DashJSURL  string
	ConfigJSON template.JS
}

// playerConfig is serialized into the page for the JavaScript runtime.
type playerConfig struct {
	StreamID   string `json:"streamId"`
	WSPath     string `json:"wsPath"`
	HLSURL     string `json:"hlsUrl"`
	DashURL    string `json:"dashUrl"`
	Delivery   string `json:"delivery"`
	LowLatency bool   `json:"lowLatency"`
}

// ServePlayer renders /player.html?id=STREAM_ID.
func (h *Handler) ServePlayer(w http.ResponseWriter, r *http.Request) {
	streamID := strings.TrimSpace(r.URL.Query().Get("id"))
	if streamID == "" {
		http.Error(w, "missing required query parameter: id", http.StatusBadRequest)
		return
	}

	mode := broadcast.DeliveryAll
	if h.cfg.Delivery != nil {
		mode = h.cfg.Delivery(streamID)
	}
	if !mode.Valid() {
		mode = broadcast.DeliveryAll
	}

	pc := playerConfig{
		StreamID:   streamID,
		WSPath:     h.cfg.WSPath,
		Delivery:   string(mode),
		LowLatency: h.cfg.LowLatency,
	}
	if mode.UsesHLS() {
		pc.HLSURL = h.mediaURL(h.cfg.HLSBase, streamID, "index.m3u8")
	}
	if mode.UsesDASH() {
		pc.DashURL = h.mediaURL(h.cfg.DashBase, streamID, "manifest.mpd")
	}

	raw, err := json.Marshal(pc)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data := viewData{
		StreamID:  streamID,
		Title:     h.cfg.Title,
		AssetBase: AssetPath,
		DashJSURL: h.cfg.DashJSURL,
		// Prevent a stream id containing "</script>" from breaking out.
		ConfigJSON: template.JS(strings.ReplaceAll(string(raw), "</", `<\/`)),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := h.tmpl.ExecuteTemplate(w, "player.html", data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// Assets serves the embedded CSS/JS bundle under AssetPath.
func (h *Handler) Assets() http.Handler {
	return http.StripPrefix("/player/", http.FileServer(http.FS(assets)))
}

// mediaURL builds a playback URL, inserting a signed token path segment when
// tokenization is enabled so relative segment requests keep the token.
func (h *Handler) mediaURL(base, streamID, file string) string {
	segment := streamID
	if h.cfg.Signer != nil && h.cfg.Signer.Enabled() {
		if token, _ := h.cfg.Signer.Sign(streamID); token != "" {
			segment = token + "/" + streamID
		}
	}
	path := strings.Trim(strings.Join([]string{strings.Trim(base, "/"), segment, file}, "/"), "/")
	if h.cfg.PublicURL == "" {
		return "/" + path
	}
	return strings.TrimRight(h.cfg.PublicURL, "/") + "/" + path
}
