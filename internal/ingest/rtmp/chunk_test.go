package rtmp

import (
	"bytes"
	"testing"
)

func TestChunkRoundTripSingleChunk(t *testing.T) {
	var buf bytes.Buffer
	w := NewChunkWriter(&buf)
	payload := []byte("hello rtmp")
	if err := w.WriteMessage(3, MsgCommandAMF0, 0, 42, payload); err != nil {
		t.Fatal(err)
	}

	r := NewChunkReader(&buf)
	msg, err := r.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if msg.CSID != 3 || msg.Type != MsgCommandAMF0 || msg.Timestamp != 42 {
		t.Fatalf("header = %+v", msg)
	}
	if !bytes.Equal(msg.Payload, payload) {
		t.Fatalf("payload = %q", msg.Payload)
	}
}

func TestChunkRoundTripMultiChunk(t *testing.T) {
	var buf bytes.Buffer
	w := NewChunkWriter(&buf)
	payload := bytes.Repeat([]byte{0xAB}, 300) // spans 3 default-size chunks
	if err := w.WriteMessage(5, MsgVideo, 1, 1000, payload); err != nil {
		t.Fatal(err)
	}

	r := NewChunkReader(&buf)
	msg, err := r.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if msg.CSID != 5 || msg.Type != MsgVideo || msg.StreamID != 1 || msg.Timestamp != 1000 {
		t.Fatalf("header = %+v", msg)
	}
	if !bytes.Equal(msg.Payload, payload) {
		t.Fatalf("payload length = %d, want %d", len(msg.Payload), len(payload))
	}
}

func TestChunkReadSequentialMessages(t *testing.T) {
	var buf bytes.Buffer
	w := NewChunkWriter(&buf)
	if err := w.WriteMessage(3, MsgCommandAMF0, 0, 1, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteMessage(3, MsgCommandAMF0, 0, 2, []byte("second")); err != nil {
		t.Fatal(err)
	}

	r := NewChunkReader(&buf)
	m1, err := r.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	m2, err := r.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if string(m1.Payload) != "first" || string(m2.Payload) != "second" {
		t.Fatalf("payloads = %q, %q", m1.Payload, m2.Payload)
	}
}
