package webrtc

import (
	"io"
	"log/slog"
	"testing"

	"github.com/pion/rtp"
)

func TestIngestAudioTrack(t *testing.T) {
	e, err := NewEngine(Config{EnableLoopback: true}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Shutdown()

	s, err := e.Ingest("demo", "s1")
	if err != nil {
		t.Fatal(err)
	}

	track, err := s.EnsureAudioTrack()
	if err != nil || track == nil {
		t.Fatalf("EnsureAudioTrack: track=%v err=%v", track, err)
	}
	if again, _ := s.EnsureAudioTrack(); again != track {
		t.Fatal("EnsureAudioTrack is not idempotent")
	}

	pkt := &rtp.Packet{
		Header:  rtp.Header{Version: 2, SequenceNumber: 1, PayloadType: 111},
		Payload: []byte{0x01, 0x02, 0x03},
	}
	if err := s.WriteAudioRTP(pkt); err != nil {
		t.Fatal(err)
	}
	if got := s.AudioPackets(); got != 1 {
		t.Fatalf("AudioPackets = %d, want 1", got)
	}

	s.Close()
}
