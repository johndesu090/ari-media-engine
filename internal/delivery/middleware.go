package delivery

import (
	"encoding/json"
	"net/http"
	"strings"
)

// StreamIDFunc extracts the target stream identifier from an incoming request.
type StreamIDFunc func(*http.Request) string

// QueryStreamID returns a StreamIDFunc that reads the named query parameter.
func QueryStreamID(param string) StreamIDFunc {
	return func(r *http.Request) string {
		return r.URL.Query().Get(param)
	}
}

// EmbedGuard enforces the embedding domain policy on embeddable endpoints. It
// always emits the frame-ancestors directive; when Enforce is true it also
// rejects requests whose Origin/Referer is not allow-listed.
type EmbedGuard struct {
	Policy   *DomainPolicy
	StreamID StreamIDFunc
	// Enforce enables server-side origin rejection in addition to the CSP
	// frame-ancestors directive that the browser enforces.
	Enforce bool
}

// Middleware wraps next with embed-security enforcement.
func (g EmbedGuard) Middleware(next http.Handler) http.Handler {
	if g.StreamID == nil {
		g.StreamID = QueryStreamID("id")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamID := g.StreamID(r)
		ancestors := "'none'"
		if g.Policy != nil {
			ancestors = g.Policy.FrameAncestors(streamID)
		}
		ApplyPlayerHeaders(w.Header(), ancestors)

		if g.Enforce && g.Policy != nil && !g.Policy.IsAllowed(streamID, requestOrigin(r)) {
			writeDenied(w, streamID)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// SecurityHeaders returns middleware that applies baseline security headers to
// non-embeddable responses (API, dashboard, static assets). When includeCSP is
// false only the baseline headers are set (no Content-Security-Policy), which is
// useful when the CSP should apply to the player page alone.
func SecurityHeaders(includeCSP bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if includeCSP {
				ApplySecurityHeaders(w.Header(), "'none'")
			} else {
				ApplyBaselineHeaders(w.Header())
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ApplyBaselineHeaders writes the hardening headers that do not depend on the
// frame-ancestors policy.
func ApplyBaselineHeaders(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
}

// ApplySecurityHeaders writes the engine's standard hardening headers plus the
// Content-Security-Policy. The frameAncestors argument is the CSP
// frame-ancestors source list.
func ApplySecurityHeaders(h http.Header, frameAncestors string) {
	applySecurityHeaders(h, frameAncestors, false)
}

// ApplyPlayerHeaders writes the hardening headers plus the CSP used for the
// embeddable player page. It additionally allows 'unsafe-eval' because the
// Clappr player compiles its UI templates with new Function; the strict policy
// is kept for the API and dashboard.
func ApplyPlayerHeaders(h http.Header, frameAncestors string) {
	applySecurityHeaders(h, frameAncestors, true)
}

func applySecurityHeaders(h http.Header, frameAncestors string, allowEval bool) {
	if frameAncestors == "" {
		frameAncestors = "'none'"
	}
	scriptSrc := "script-src 'self'"
	if allowEval {
		scriptSrc = "script-src 'self' 'unsafe-eval'"
	}
	csp := strings.Join([]string{
		"default-src 'self'",
		"img-src 'self' data: blob:",
		"media-src 'self' blob:",
		"connect-src 'self' ws: wss:",
		scriptSrc,
		"worker-src 'self' blob:",
		"child-src 'self' blob:",
		"style-src 'self' 'unsafe-inline'",
		"base-uri 'self'",
		"form-action 'self'",
		"frame-ancestors " + frameAncestors,
	}, "; ")

	h.Set("Content-Security-Policy", csp)
	ApplyBaselineHeaders(h)
	h.Set("Cross-Origin-Resource-Policy", "cross-origin")

	switch frameAncestors {
	case "'none'":
		h.Set("X-Frame-Options", "DENY")
	case "'self'":
		h.Set("X-Frame-Options", "SAMEORIGIN")
	case WildcardAll:
		// No legacy header can express a wildcard; rely on CSP.
		h.Del("X-Frame-Options")
	default:
		// A specific allow-list cannot be expressed by X-Frame-Options; omit it
		// so allowed embedders are not blocked, and let CSP do the enforcement.
		h.Del("X-Frame-Options")
	}
}

// requestOrigin returns the Origin header when present, otherwise the Referer.
func requestOrigin(r *http.Request) string {
	if o := r.Header.Get("Origin"); o != "" {
		return o
	}
	return r.Header.Get("Referer")
}

func writeDenied(w http.ResponseWriter, streamID string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":    "embedding_denied",
		"message":  "this domain is not allowed to embed the stream",
		"streamId": streamID,
	})
}
