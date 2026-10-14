package delivery

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TokenSigner mints and verifies signed playback tokens for HLS/DASH URLs. A
// token authorises access to a single stream until an expiry time. Tokens are
// stateless: "<unixExpiry>.<hexHMAC(streamID:expiry)>".
type TokenSigner struct {
	mu      sync.RWMutex
	enabled bool
	secret  []byte
	ttl     time.Duration
}

// NewTokenSigner builds a signer. The signer is active only when enabled is true
// and a non-empty secret is configured.
func NewTokenSigner(enabled bool, secret string, ttl time.Duration) *TokenSigner {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &TokenSigner{enabled: enabled, secret: []byte(secret), ttl: ttl}
}

// Enabled reports whether token enforcement is active.
func (t *TokenSigner) Enabled() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.enabled && len(t.secret) > 0
}

// SetEnabled toggles token enforcement.
func (t *TokenSigner) SetEnabled(v bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.enabled = v
}

// SetSecret replaces the signing secret.
func (t *TokenSigner) SetSecret(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.secret = []byte(s)
}

// EnsureSecret generates a random signing secret when none is configured.
func (t *TokenSigner) EnsureSecret() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.secret) > 0 {
		return
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err == nil {
		t.secret = []byte(hex.EncodeToString(b))
	}
}

// TTL returns the token lifetime.
func (t *TokenSigner) TTL() time.Duration {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.ttl
}

// SetTTL updates the token lifetime.
func (t *TokenSigner) SetTTL(d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if d > 0 {
		t.ttl = d
	}
}

// Sign returns a fresh token for streamID and its expiry time.
func (t *TokenSigner) Sign(streamID string) (string, time.Time) {
	t.mu.RLock()
	secret := append([]byte(nil), t.secret...)
	ttl := t.ttl
	t.mu.RUnlock()

	exp := time.Now().Add(ttl).UTC()
	return encodeToken(secret, streamID, exp), exp
}

// Verify reports whether token is authentic for streamID and not expired.
func (t *TokenSigner) Verify(streamID, token string) bool {
	t.mu.RLock()
	secret := append([]byte(nil), t.secret...)
	t.mu.RUnlock()
	if len(secret) == 0 || token == "" {
		return false
	}

	exp, ok := decodeExpiry(token)
	if !ok || time.Now().After(exp) {
		return false
	}
	want := encodeToken(secret, streamID, exp)
	return hmac.Equal([]byte(want), []byte(token))
}

func encodeToken(secret []byte, streamID string, exp time.Time) string {
	payload := strconv.FormatInt(exp.Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(streamID))
	mac.Write([]byte{':'})
	mac.Write([]byte(payload))
	return payload + "." + hex.EncodeToString(mac.Sum(nil))
}

func decodeExpiry(token string) (time.Time, bool) {
	i := strings.IndexByte(token, '.')
	if i <= 0 {
		return time.Time{}, false
	}
	secs, err := strconv.ParseInt(token[:i], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(secs, 0).UTC(), true
}
