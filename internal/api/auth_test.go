package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/johndesu090/ari-media-engine/internal/auth"
	"github.com/johndesu090/ari-media-engine/internal/config"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}

func TestLoginAndSessionCookie(t *testing.T) {
	s := &Server{
		auth:     config.AuthConfig{Enabled: true, Username: "admin", Password: "secret"},
		sessions: auth.NewStore(time.Minute),
	}

	// Wrong password is rejected.
	rec := httptest.NewRecorder()
	s.handleLogin(rec, httptest.NewRequest(http.MethodPost, "/api/v1/login", strings.NewReader(`{"username":"admin","password":"nope"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad login status = %d, want 401", rec.Code)
	}

	// Correct credentials set a session cookie.
	rec = httptest.NewRecorder()
	s.handleLogin(rec, httptest.NewRequest(http.MethodPost, "/api/v1/login", strings.NewReader(`{"username":"admin","password":"secret"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200", rec.Code)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("no session cookie set")
	}

	// The session cookie authorises control-plane routes.
	protected := s.requireAuth(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
	req.AddCookie(cookies[0])
	rec = httptest.NewRecorder()
	protected.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authorized status = %d, want 200", rec.Code)
	}

	// Without a cookie the route is protected.
	rec = httptest.NewRecorder()
	protected.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want 401", rec.Code)
	}
}

func TestAPIKeyStillAuthorises(t *testing.T) {
	s := &Server{
		auth:     config.AuthConfig{Enabled: true, APIKey: "key"},
		sessions: auth.NewStore(time.Minute),
	}
	protected := s.requireAuth(okHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
	req.Header.Set("X-API-Key", "key")
	rec := httptest.NewRecorder()
	protected.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("api key status = %d, want 200", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
	req.Header.Set("Authorization", "Bearer key")
	rec = httptest.NewRecorder()
	protected.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bearer status = %d, want 200", rec.Code)
	}
}

func TestAuthDisabledAllowsAll(t *testing.T) {
	s := &Server{auth: config.AuthConfig{Enabled: false}, sessions: auth.NewStore(time.Minute)}
	rec := httptest.NewRecorder()
	s.requireAuth(okHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("disabled-auth status = %d, want 200", rec.Code)
	}
}
