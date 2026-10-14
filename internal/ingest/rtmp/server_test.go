package rtmp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

type fakeSink struct {
	mu        sync.Mutex
	sps, pps  []byte
	videos    [][]byte
	keyframes []bool
	closed    bool
}

func (f *fakeSink) SetVideoConfig(sps, pps []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sps = append([]byte(nil), sps...)
	f.pps = append([]byte(nil), pps...)
	return nil
}

func (f *fakeSink) WriteVideo(nalus [][]byte, _ uint32, keyframe bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range nalus {
		f.videos = append(f.videos, append([]byte(nil), n...))
		f.keyframes = append(f.keyframes, keyframe)
	}
	return nil
}

func (f *fakeSink) Close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
}

type fakeFactory struct{ sink *fakeSink }

func (f *fakeFactory) Open(string) (MediaSink, error) { return f.sink, nil }

func TestServerPublishFlow(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	sink := &fakeSink{}
	srv := &Server{Sinks: &fakeFactory{sink: sink}, ReadTimeout: 5 * time.Second}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, server) }()

	if err := clientHandshake(client); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))

	cr := NewChunkReader(client)
	cw := NewChunkWriter(client)

	if err := sendCommand(cw, "connect", 1, map[string]any{"app": "live"}); err != nil {
		t.Fatal(err)
	}
	if _, err := readCommand(cr, "_result"); err != nil {
		t.Fatalf("connect result: %v", err)
	}

	if err := sendCommand(cw, "createStream", 2, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := readCommand(cr, "_result"); err != nil {
		t.Fatalf("createStream result: %v", err)
	}

	if err := sendCommand(cw, "publish", 3, nil, "demo", "live"); err != nil {
		t.Fatal(err)
	}
	status, err := readCommand(cr, "onStatus")
	if err != nil {
		t.Fatalf("publish status: %v", err)
	}
	if len(status) < 4 {
		t.Fatalf("status = %v", status)
	}

	// AVC sequence header.
	avc := buildAVCConfig([]byte{0x67, 0x42, 0xE0, 0x1F, 0xDA}, []byte{0x68, 0xCE, 0x3C})
	seq := append([]byte{0x17, 0x00, 0x00, 0x00, 0x00}, avc...)
	if err := cw.WriteMessage(4, MsgVideo, 1, 0, seq); err != nil {
		t.Fatal(err)
	}

	// Keyframe access unit.
	var sample bytes.Buffer
	sample.Write([]byte{0x00, 0x00, 0x00, 0x03, 0x65, 0xAA, 0xBB})
	nalu := append([]byte{0x17, 0x01, 0x00, 0x00, 0x00}, sample.Bytes()...)
	if err := cw.WriteMessage(4, MsgVideo, 1, 40, nalu); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		sink.mu.Lock()
		ready := len(sink.videos) > 0 && sink.sps != nil
		sink.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for demuxed media")
		}
		time.Sleep(10 * time.Millisecond)
	}

	sink.mu.Lock()
	if !bytes.Equal(sink.sps, []byte{0x67, 0x42, 0xE0, 0x1F, 0xDA}) {
		t.Fatalf("sps = %x", sink.sps)
	}
	if len(sink.videos) != 1 || !bytes.Equal(sink.videos[0], []byte{0x65, 0xAA, 0xBB}) {
		t.Fatalf("videos = %x", sink.videos)
	}
	if !sink.keyframes[0] {
		t.Fatal("expected the keyframe flag to be set")
	}
	sink.mu.Unlock()

	_ = client.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop after client close")
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if !sink.closed {
		t.Fatal("sink was not closed")
	}
}

func clientHandshake(conn net.Conn) error {
	c0c1 := make([]byte, 1+handshakeSize)
	c0c1[0] = handshakeVersion
	if _, err := rand.Read(c0c1[1:]); err != nil {
		return err
	}
	if _, err := conn.Write(c0c1); err != nil {
		return err
	}
	s0s1s2 := make([]byte, 1+2*handshakeSize)
	if _, err := io.ReadFull(conn, s0s1s2); err != nil {
		return err
	}
	_, err := conn.Write(s0s1s2[1 : 1+handshakeSize])
	return err
}

func sendCommand(w *ChunkWriter, name string, txn float64, args ...any) error {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	if err := enc.WriteString(name); err != nil {
		return err
	}
	if err := enc.WriteNumber(txn); err != nil {
		return err
	}
	for _, a := range args {
		if err := enc.WriteValue(a); err != nil {
			return err
		}
	}
	return w.WriteMessage(3, MsgCommandAMF0, 0, 0, buf.Bytes())
}

func readCommand(r *ChunkReader, name string) ([]any, error) {
	for {
		msg, err := r.ReadMessage()
		if err != nil {
			return nil, err
		}
		if msg.Type == MsgSetChunkSize && len(msg.Payload) >= 4 {
			r.SetChunkSize(binary.BigEndian.Uint32(msg.Payload[:4]) & 0x7fffffff)
			continue
		}
		if msg.Type != MsgCommandAMF0 {
			continue
		}
		dec := NewDecoder(bytes.NewReader(msg.Payload))
		var vals []any
		for {
			v, err := dec.Decode()
			if err != nil {
				break
			}
			vals = append(vals, v)
		}
		if len(vals) > 0 {
			if n, _ := vals[0].(string); n == name {
				return vals, nil
			}
		}
	}
}
