package delivery

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHostFromOrigin(t *testing.T) {
	cases := map[string]string{
		"https://example.com":            "example.com",
		"https://example.com:8443":       "example.com",
		"http://Sub.Example.COM.":        "sub.example.com",
		"example.com":                    "example.com",
		"example.com:1935":               "example.com",
		"//cdn.example.com/path":         "cdn.example.com",
		"https://example.com/page?x=1#y": "example.com",
		"":                               "",
		"not a url with spaces":          "not a url with spaces",
	}
	for in, want := range cases {
		if got := HostFromOrigin(in); got != want {
			t.Errorf("HostFromOrigin(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsAllowed(t *testing.T) {
	p := NewDomainPolicy([]string{"example.com", "*.trusted.net", "https://partner.io"}, false)

	allow := []string{
		"", // direct navigation
		"https://example.com",
		"https://example.com:443/app",
		"https://a.trusted.net",
		"https://deep.a.trusted.net",
		"https://partner.io",
	}
	for _, o := range allow {
		if !p.IsAllowed("s1", o) {
			t.Errorf("expected %q allowed", o)
		}
	}

	deny := []string{
		"https://evil.com",
		"https://trusted.net", // apex not matched by *.trusted.net
		"https://example.com.evil.com",
		"https://notpartner.io",
	}
	for _, o := range deny {
		if p.IsAllowed("s1", o) {
			t.Errorf("expected %q denied", o)
		}
	}
}

func TestPerStreamOverride(t *testing.T) {
	p := NewDomainPolicy([]string{"global.com"}, false)
	p.SetStream("s1", []string{"special.com"})

	if !p.IsAllowed("s1", "https://special.com") {
		t.Error("per-stream domain should be allowed")
	}
	if p.IsAllowed("s1", "https://global.com") {
		t.Error("per-stream override should replace the global list")
	}
	if !p.IsAllowed("s2", "https://global.com") {
		t.Error("other streams should use the global list")
	}

	p.SetStream("s1", nil)
	if !p.IsAllowed("s1", "https://global.com") {
		t.Error("clearing the override should fall back to global")
	}
}

func TestAllowAll(t *testing.T) {
	p := NewDomainPolicy(nil, true)
	if !p.IsAllowed("s1", "https://anything.example") {
		t.Error("allow-all should permit any origin")
	}
	if got := p.FrameAncestors("s1"); got != WildcardAll {
		t.Errorf("FrameAncestors = %q, want %q", got, WildcardAll)
	}
}

func TestFrameAncestors(t *testing.T) {
	p := NewDomainPolicy([]string{"b.com", "a.com"}, false)
	got := p.FrameAncestors("s1")
	want := "'self' a.com b.com"
	if got != want {
		t.Errorf("FrameAncestors = %q, want %q", got, want)
	}
}

func TestEmbedGuardDeniesForeignOrigin(t *testing.T) {
	p := NewDomainPolicy([]string{"allowed.com"}, false)
	guard := EmbedGuard{Policy: p, StreamID: QueryStreamID("id"), Enforce: true}

	called := false
	h := guard.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/player.html?id=live1", nil)
	req.Header.Set("Origin", "https://evil.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if called {
		t.Fatal("downstream handler should not run for denied origin")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got == "" {
		t.Fatal("expected a Content-Security-Policy header")
	}
}

func TestEmbedGuardAllowsListedOrigin(t *testing.T) {
	p := NewDomainPolicy([]string{"allowed.com"}, false)
	guard := EmbedGuard{Policy: p, StreamID: QueryStreamID("id"), Enforce: true}

	h := guard.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/player.html?id=live1", nil)
	req.Header.Set("Origin", "https://allowed.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got == "" {
		t.Fatal("expected a Content-Security-Policy header")
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "" {
		t.Fatalf("X-Frame-Options = %q, want empty for an allow-list", got)
	}
}
