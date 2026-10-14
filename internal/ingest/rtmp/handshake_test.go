package rtmp

import (
	"bytes"
	"crypto/rand"
	"io"
	"net"
	"testing"
)

func TestServerHandshake(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	errCh := make(chan error, 1)
	go func() { errCh <- ServerHandshake(server) }()

	// Client C0 + C1.
	c0c1 := make([]byte, 1+handshakeSize)
	c0c1[0] = handshakeVersion
	if _, err := rand.Read(c0c1[1:]); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write(c0c1); err != nil {
		t.Fatal(err)
	}

	// Read S0 + S1 + S2.
	s0s1s2 := make([]byte, 1+2*handshakeSize)
	if _, err := io.ReadFull(client, s0s1s2); err != nil {
		t.Fatal(err)
	}
	if s0s1s2[0] != handshakeVersion {
		t.Fatalf("S0 version = %d", s0s1s2[0])
	}
	if !bytes.Equal(s0s1s2[1+handshakeSize:], c0c1[1:]) {
		t.Fatal("S2 should echo C1")
	}

	// Client C2 (echo of S1).
	if _, err := client.Write(s0s1s2[1 : 1+handshakeSize]); err != nil {
		t.Fatal(err)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("server handshake: %v", err)
	}
}

func TestServerHandshakeRejectsBadVersion(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	errCh := make(chan error, 1)
	go func() { errCh <- ServerHandshake(server) }()

	bad := make([]byte, 1+handshakeSize)
	bad[0] = 0x02
	if _, err := client.Write(bad); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err == nil {
		t.Fatal("expected an error for an unsupported handshake version")
	}
}
