package delivery

import (
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// PlaybackServer serves HLS/DASH files from Root.
//
// When the TokenSigner is enabled, the token is the first URL path segment:
//
//	/hls/<token>/<streamID>/index.m3u8
//
// so that relative playlist/segment URLs keep the token. When tokenization is
// disabled the stream id is the first segment: /hls/<streamID>/index.m3u8.
type PlaybackServer struct {
	Root   string
	Prefix string
	Signer *TokenSigner
	Log    *slog.Logger
}

// ServeHTTP resolves the request against Root, enforcing a token when required.
func (p *PlaybackServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, p.Prefix)
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" {
		http.NotFound(w, r)
		return
	}

	parts := strings.SplitN(rel, "/", 2)
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}

	streamID := parts[0]
	filePath := parts[1]

	if p.Signer != nil && p.Signer.Enabled() {
		token := parts[0]
		inner := strings.SplitN(parts[1], "/", 2)
		if len(inner) < 2 {
			writeTokenDenied(w)
			return
		}
		streamID = inner[0]
		filePath = inner[1]
		if !p.Signer.Verify(streamID, token) {
			writeTokenDenied(w)
			return
		}
	}

	clean := strings.TrimPrefix(path.Clean("/"+filePath), "/")
	if clean == "" || clean == "." || strings.HasPrefix(clean, "..") {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	root := p.Root
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	base := filepath.Join(root, filepath.FromSlash(streamID))
	full := filepath.Join(base, filepath.FromSlash(clean))
	if !withinBase(base, full) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	f, err := os.Open(full)
	if err != nil {
		if p.Log != nil {
			p.Log.Debug("playback file open failed", "path", full, "err", err)
		}
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	if ct := contentTypeFor(fi.Name()); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
}

// contentTypeFor returns the explicit MIME type for HLS/DASH artifacts. Go's
// default extension table does not know .m3u8/.ts, and Safari refuses to play
// HLS unless the playlist is served as application/vnd.apple.mpegurl and the
// segments as video/mp2t.
func contentTypeFor(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".m3u8":
		return "application/vnd.apple.mpegurl"
	case ".ts":
		return "video/mp2t"
	case ".mpd":
		return "application/dash+xml"
	case ".m4s", ".mp4", ".m4v":
		return "video/mp4"
	case ".m4a":
		return "audio/mp4"
	case ".vtt":
		return "text/vtt"
	default:
		return ""
	}
}

func withinBase(base, full string) bool {
	rel, err := filepath.Rel(base, full)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func writeTokenDenied(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"token_required","message":"a valid playback token is required"}`))
}
