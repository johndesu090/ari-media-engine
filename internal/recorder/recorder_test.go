package recorder

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/johndesu090/ari-media-engine/internal/storage"
)

func TestRecorderWritesAndUploads(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.NewLocalStore(filepath.Join(dir, "objects"), "")
	if err != nil {
		t.Fatal(err)
	}

	m := New(Options{
		Store:   store,
		WorkDir: filepath.Join(dir, "work"),
		Prefix:  "recordings",
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	sess, err := m.Start("demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.WriteFLVTag(9, 0, []byte{0x17, 0x00, 0x00, 0x00, 0x00}); err != nil {
		t.Fatal(err)
	}
	if err := sess.WriteFLVTag(9, 40, []byte{0x17, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03, 0x65, 0xAA, 0xBB}); err != nil {
		t.Fatal(err)
	}
	if len(m.Active()) != 1 {
		t.Fatalf("active = %d, want 1", len(m.Active()))
	}
	m.Stop("demo")

	objs, err := store.List(context.Background(), "recordings/")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 {
		t.Fatalf("objects = %d, want 1", len(objs))
	}
	b, err := os.ReadFile(filepath.Join(dir, "objects", filepath.FromSlash(objs[0].Key)))
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 3 || string(b[0:3]) != "FLV" {
		t.Fatalf("uploaded file is not FLV: % x", b[:min(9, len(b))])
	}
}
