// Package auth provides a small in-memory session store for the dashboard
// login. Sessions are random tokens mapped to an expiry; they are presented as
// an HttpOnly cookie and validated on control-plane requests.
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// CookieName is the dashboard session cookie name.
const CookieName = "ari_session"

// Store is a concurrency-safe set of session tokens.
type Store struct {
	mu       sync.Mutex
	sessions map[string]time.Time
	ttl      time.Duration
}

// NewStore returns a session store whose tokens live for ttl (default 12h).
func NewStore(ttl time.Duration) *Store {
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	return &Store{sessions: map[string]time.Time{}, ttl: ttl}
}

// TTL returns the session lifetime.
func (s *Store) TTL() time.Duration { return s.ttl }

// Create mints a new session token.
func (s *Store) Create() string {
	token := randomToken()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[token] = time.Now().Add(s.ttl)
	s.gcLocked()
	return token
}

// Valid reports whether token is a live session.
func (s *Store) Valid(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.sessions, token)
		return false
	}
	return true
}

// Delete removes a session (logout).
func (s *Store) Delete(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, token)
}

// gcLocked drops expired sessions. The caller must hold the lock.
func (s *Store) gcLocked() {
	now := time.Now()
	for token, exp := range s.sessions {
		if now.After(exp) {
			delete(s.sessions, token)
		}
	}
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("auth: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}
