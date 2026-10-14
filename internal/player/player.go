// Package player implements the Ari Media Engine built-in HTML5 video player.
// It renders a self-hosted, embeddable page at /player.html?id=STREAM_ID and
// serves its CSS/JS assets. The page plays the stream over HLS, falling back to
// DASH. When tokenization is enabled the page refreshes its signed token
// automatically a few seconds before it expires.
package player

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/johndesu090/ari-media-engine/internal/broadcast"
	"github.com/johndesu090/ari-media-engine/internal/delivery"
)

//go:embed assets
var assets embed.FS

// AssetPath is the URL prefix under which player assets are served.
const AssetPath = "/player/assets"

// TokenPath is the public endpoint the player calls to refresh its token.
const TokenPath = "/player/token"

// tokenRefreshLead is how long before expiry the player refreshes its token.
const tokenRefreshLead = 5 * time.Second

// Config configures the player renderer.
type Config struct {
	// Title is shown in the page title and as the default UI label.
	Title string
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
	// ClapprURL optionally loads a Clappr build for HLS playback (it bundles
	// hls.js and falls back to native playback on iOS).
	ClapprURL string
	// HLSJSURL is retained for compatibility; Clappr provides hls.js.
	HLSJSURL string
	// P2PEnabled turns on the peer-assisted delivery engine.
	P2PEnabled bool
	// P2PWsPath is the tracker WebSocket path (e.g. /p2p/ws).
	P2PWsPath string
	// Signer mints playback tokens for HLS/DASH URLs when tokenization is on.
	Signer *delivery.TokenSigner
	// Delivery resolves the delivery mode for a stream id.
	Delivery func(streamID string) broadcast.Delivery
}

// Handler renders the player page and serves its static assets.
type Handler struct {
	tmpl         *template.Template
	cfg          Config
	assetVersion string
}

// New parses the embedded templates and returns a Handler.
func New(cfg Config) (*Handler, error) {
	// Default to the bundled, self-hosted hls.js / dash.js so HLS and DASH
	// playback work without a CDN (which the CSP would block).
	if cfg.HLSJSURL == "" {
		cfg.HLSJSURL = AssetPath + "/vendor/hls.min.js"
	}
	if cfg.DashJSURL == "" {
		cfg.DashJSURL = AssetPath + "/vendor/dash.all.min.js"
	}
	if cfg.ClapprURL == "" {
		cfg.ClapprURL = AssetPath + "/vendor/clappr.min.js"
	}
	// Cache-bust the bundled assets so an updated player is never served stale
	// against a newer page (or vice versa).
	ver := assetVersion()
	cfg.HLSJSURL = versionAsset(cfg.HLSJSURL, ver)
	cfg.DashJSURL = versionAsset(cfg.DashJSURL, ver)
	cfg.ClapprURL = versionAsset(cfg.ClapprURL, ver)
	tmpl, err := template.ParseFS(assets, "assets/*.html")
	if err != nil {
		return nil, fmt.Errorf("player: parse templates: %w", err)
	}
	return &Handler{tmpl: tmpl, cfg: cfg, assetVersion: ver}, nil
}

// assetVersion derives a short content hash of the player bundle so asset URLs
// change whenever the bundle changes.
func assetVersion() string {
	h := sha256.New()
	for _, name := range []string{
		"assets/player.js", "assets/player.css", "assets/player.html",
		"assets/vendor/clappr.min.js", "assets/vendor/hls.min.js", "assets/vendor/dash.all.min.js",
	} {
		if b, err := assets.ReadFile(name); err == nil {
			h.Write(b)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// versionAsset appends the content version to bundled asset URLs (only those
// served from AssetPath), leaving external/CDN URLs untouched.
func versionAsset(u, ver string) string {
	if u == "" || !strings.HasPrefix(u, AssetPath+"/") || strings.Contains(u, "?") {
		return u
	}
	return u + "?v=" + ver
}

// viewData is the template payload.
type viewData struct {
	StreamID     string
	Title        string
	AssetBase    string
	AssetVersion string
	DashJSURL    string
	ClapprURL    string
	HLSJSURL     string
	P2PEnabled   bool
	ConfigJSON   template.JS
}

// playerConfig is serialized into the page for the JavaScript runtime.
type playerConfig struct {
	StreamID       string    `json:"streamId"`
	HLSURL         string    `json:"hlsUrl"`
	DashURL        string    `json:"dashUrl"`
	Delivery       string    `json:"delivery"`
	LowLatency     bool      `json:"lowLatency"`
	TokenEnabled   bool      `json:"tokenEnabled"`
	Token          string    `json:"token,omitempty"`
	TokenExpiresAt int64     `json:"tokenExpiresAt,omitempty"`
	TokenPath      string    `json:"tokenPath,omitempty"`
	RefreshLeadMs  int64     `json:"refreshLeadMs"`
	P2P            p2pConfig `json:"p2p"`
}

type p2pConfig struct {
	Enabled bool   `json:"enabled"`
	WSPath  string `json:"wsPath"`
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
		StreamID:      streamID,
		Delivery:      string(mode),
		LowLatency:    h.cfg.LowLatency,
		P2P:           p2pConfig{Enabled: h.cfg.P2PEnabled, WSPath: h.cfg.P2PWsPath},
		TokenPath:     TokenPath,
		RefreshLeadMs: tokenRefreshLead.Milliseconds(),
	}
	// Build playback URLs on the public base, but fall back to same-origin
	// relative URLs when the configured base would be mixed content (an http
	// origin on an https page), which the browser and CSP would block.
	publicBase := h.cfg.PublicURL
	if publicBase != "" && requestHTTPS(r) && strings.HasPrefix(strings.ToLower(publicBase), "http://") {
		publicBase = ""
	}

	var token string
	if h.cfg.Signer != nil && h.cfg.Signer.Enabled() {
		pc.TokenEnabled = true
		tok, exp := h.cfg.Signer.Sign(streamID)
		token = tok
		pc.Token = tok
		pc.TokenExpiresAt = exp.UnixMilli()
	}

	if mode.UsesHLS() {
		pc.HLSURL = h.mediaURL(publicBase, h.cfg.HLSBase, streamID, "index.m3u8", token)
	}
	if mode.UsesDASH() {
		pc.DashURL = h.mediaURL(publicBase, h.cfg.DashBase, streamID, "manifest.mpd", token)
	}

	raw, err := json.Marshal(pc)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data := viewData{
		StreamID:     streamID,
		Title:        h.cfg.Title,
		AssetBase:    AssetPath,
		AssetVersion: h.assetVersion,
		DashJSURL:    h.cfg.DashJSURL,
		ClapprURL:    h.cfg.ClapprURL,
		HLSJSURL:     h.cfg.HLSJSURL,
		P2PEnabled:   h.cfg.P2PEnabled,
		// Prevent a stream id containing "</script>" from breaking out.
		ConfigJSON: template.JS(strings.ReplaceAll(string(raw), "</", `<\/`)),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := h.tmpl.ExecuteTemplate(w, "player.html", data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// ServeToken mints a fresh playback token for a stream so the player can
// refresh before its current token expires. It is a public endpoint intended to
// sit behind the same embed/domain guard as the player page; when tokenization
// is disabled it reports that no token is required.
func (h *Handler) ServeToken(w http.ResponseWriter, r *http.Request) {
	streamID := strings.TrimSpace(r.URL.Query().Get("id"))
	if streamID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing required query parameter: id"})
		return
	}
	if h.cfg.Signer == nil || !h.cfg.Signer.Enabled() {
		writeJSON(w, http.StatusOK, map[string]any{"tokenEnabled": false})
		return
	}

	mode := broadcast.DeliveryAll
	if h.cfg.Delivery != nil {
		mode = h.cfg.Delivery(streamID)
	}
	if !mode.Valid() {
		mode = broadcast.DeliveryAll
	}

	publicBase := h.cfg.PublicURL
	if publicBase != "" && requestHTTPS(r) && strings.HasPrefix(strings.ToLower(publicBase), "http://") {
		publicBase = ""
	}

	token, exp := h.cfg.Signer.Sign(streamID)
	resp := map[string]any{
		"tokenEnabled":  true,
		"token":         token,
		"expiresAt":     exp.UnixMilli(),
		"refreshLeadMs": tokenRefreshLead.Milliseconds(),
	}
	if mode.UsesHLS() {
		resp["hlsUrl"] = h.mediaURL(publicBase, h.cfg.HLSBase, streamID, "index.m3u8", token)
	}
	if mode.UsesDASH() {
		resp["dashUrl"] = h.mediaURL(publicBase, h.cfg.DashBase, streamID, "manifest.mpd", token)
	}
	writeJSON(w, http.StatusOK, resp)
}

// Assets serves the embedded CSS/JS bundle under AssetPath. Versioned requests
// (with a ?v= query) are cached immutably; unversioned requests revalidate so a
// stale bundle is never served against a newer page.
func (h *Handler) Assets() http.Handler {
	fs := http.StripPrefix("/player/", http.FileServer(http.FS(assets)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fs.ServeHTTP(w, r)
	})
}

// mediaURL builds a playback URL, inserting a signed token path segment when
// tokenization is enabled so relative segment requests keep the token. When
// publicBase is empty a same-origin relative URL is returned.
func (h *Handler) mediaURL(publicBase, base, streamID, file, token string) string {
	segment := streamID
	if token != "" {
		segment = token + "/" + streamID
	}
	path := strings.Trim(strings.Join([]string{strings.Trim(base, "/"), segment, file}, "/"), "/")
	if publicBase == "" {
		return "/" + path
	}
	return strings.TrimRight(publicBase, "/") + "/" + path
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// requestHTTPS reports whether the player page was served over TLS (directly or
// via a reverse proxy).
func requestHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
