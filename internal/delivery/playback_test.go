package delivery

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writePlaylist(t *testing.T, root, streamID, name string) {
	t.Helper()
	dir := filepath.Join(root, streamID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#EXTM3U\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPlaybackServerTokenized(t *testing.T) {
	root := t.TempDir()
	writePlaylist(t, root, "demo", "index.m3u8")

	signer := NewTokenSigner(true, "topsecret", time.Minute)
	srv := &PlaybackServer{Root: root, Prefix: "/hls", Signer: signer}
	tok, _ := signer.Sign("demo")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("GET", "/hls/demo/index.m3u8", nil))
	if rec.Code != 401 {
		t.Fatalf("missing token status = %d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("GET", "/hls/"+tok+"/demo/index.m3u8", nil))
	if rec.Code != 200 {
		t.Fatalf("valid token status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "#EXTM3U") {
		t.Fatalf("body = %q", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("GET", "/hls/badtoken/demo/index.m3u8", nil))
	if rec.Code != 401 {
		t.Fatalf("bad token status = %d, want 401", rec.Code)
	}

	// A token for one stream must not unlock another.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("GET", "/hls/"+tok+"/other/index.m3u8", nil))
	if rec.Code != 401 {
		t.Fatalf("cross-stream token status = %d, want 401", rec.Code)
	}
}

func TestPlaybackServerOpen(t *testing.T) {
	root := t.TempDir()
	writePlaylist(t, root, "demo", "index.m3u8")

	srv := &PlaybackServer{Root: root, Prefix: "/hls"}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("GET", "/hls/demo/index.m3u8", nil))
	if rec.Code != 200 {
		t.Fatalf("open status = %d, want 200", rec.Code)
	}
}

func TestPlaybackServerRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	// A secret file outside the stream directory.
	if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	writePlaylist(t, root, "demo", "index.m3u8")

	srv := &PlaybackServer{Root: root, Prefix: "/hls"}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest("GET", "/hls/demo/../secret.txt", nil))
	if rec.Code == 200 {
		t.Fatalf("traversal served the file: %q", rec.Body.String())
	}
}
