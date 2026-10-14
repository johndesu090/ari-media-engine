package auth

import (
	"testing"
	"time"
)

func TestStoreLifecycle(t *testing.T) {
	s := NewStore(time.Minute)

	tok := s.Create()
	if tok == "" {
		t.Fatal("empty token")
	}
	if !s.Valid(tok) {
		t.Fatal("fresh token should be valid")
	}
	s.Delete(tok)
	if s.Valid(tok) {
		t.Fatal("deleted token is still valid")
	}
	if s.Valid("") || s.Valid("unknown") {
		t.Fatal("empty/unknown token accepted")
	}
}

func TestStoreExpiry(t *testing.T) {
	s := NewStore(10 * time.Millisecond)
	tok := s.Create()
	time.Sleep(30 * time.Millisecond)
	if s.Valid(tok) {
		t.Fatal("expired token accepted")
	}
}
