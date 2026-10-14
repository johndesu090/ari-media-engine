package flv

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestWriterFraming(t *testing.T) {
	var buf bytes.Buffer
	f := NewWriter(&buf)
	if err := f.WriteTag(9, 0, []byte{0x17, 0x00, 0x00, 0x00, 0x00}); err != nil {
		t.Fatal(err)
	}

	out := buf.Bytes()
	want := 9 + 4 + 11 + 5 + 4
	if len(out) != want {
		t.Fatalf("len = %d, want %d", len(out), want)
	}
	if string(out[0:3]) != "FLV" {
		t.Fatalf("signature = %q", out[0:3])
	}
	if out[4] != 0x05 {
		t.Fatalf("flags = %#x, want 0x05", out[4])
	}
	tag := out[13:]
	if tag[0] != 9 {
		t.Fatalf("tag type = %d, want 9", tag[0])
	}
	if got := int(tag[1])<<16 | int(tag[2])<<8 | int(tag[3]); got != 5 {
		t.Fatalf("data size = %d, want 5", got)
	}
	if got := binary.BigEndian.Uint32(out[len(out)-4:]); got != 16 {
		t.Fatalf("previous tag size = %d, want 16", got)
	}
}

func TestWriterHeaderWrittenOnce(t *testing.T) {
	var buf bytes.Buffer
	f := NewWriter(&buf)
	if err := f.WriteTag(9, 0, []byte{1}); err != nil {
		t.Fatal(err)
	}
	first := buf.Len()
	if err := f.WriteTag(8, 0, []byte{2}); err != nil {
		t.Fatal(err)
	}
	if got := buf.Len() - first; got != 11+1+4 {
		t.Fatalf("second tag size = %d, want %d", got, 11+1+4)
	}
	if !f.Started() {
		t.Fatal("Started() = false after writing")
	}
}
