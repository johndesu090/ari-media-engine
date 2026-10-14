package delivery

import (
	"testing"
	"time"
)

func TestTokenSignVerify(t *testing.T) {
	s := NewTokenSigner(true, "topsecret", time.Minute)
	tok, exp := s.Sign("demo")

	if !s.Verify("demo", tok) {
		t.Fatal("valid token rejected")
	}
	if s.Verify("other", tok) {
		t.Fatal("token accepted for the wrong stream")
	}
	if s.Verify("demo", tok+"x") {
		t.Fatal("tampered token accepted")
	}
	if s.Verify("demo", "") {
		t.Fatal("empty token accepted")
	}
	if exp.Before(time.Now()) {
		t.Fatal("expiry is in the past")
	}
}

func TestTokenExpiry(t *testing.T) {
	s := NewTokenSigner(true, "topsecret", time.Minute)
	expired := encodeToken([]byte("topsecret"), "demo", time.Now().Add(-time.Minute))
	if s.Verify("demo", expired) {
		t.Fatal("expired token accepted")
	}
}

func TestTokenDisabled(t *testing.T) {
	s := NewTokenSigner(false, "topsecret", time.Minute)
	if s.Enabled() {
		t.Fatal("disabled signer reports enabled")
	}
	if NewTokenSigner(true, "", time.Minute).Enabled() {
		t.Fatal("signer without a secret reports enabled")
	}
}
