package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalStore(t *testing.T) {
	dir := t.TempDir()
	s, err := NewLocalStore(dir, "https://cdn.example.com")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := s.Put(ctx, "recordings/demo/a.flv", strings.NewReader("hello"), 5, "video/x-flv"); err != nil {
		t.Fatal(err)
	}

	objs, err := s.List(ctx, "recordings/")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 {
		t.Fatalf("objects = %d, want 1", len(objs))
	}
	if objs[0].Key != "recordings/demo/a.flv" || objs[0].Size != 5 {
		t.Fatalf("object = %+v", objs[0])
	}
	if objs[0].URL != "https://cdn.example.com/recordings/demo/a.flv" {
		t.Fatalf("url = %q", objs[0].URL)
	}

	b, err := os.ReadFile(filepath.Join(dir, "recordings", "demo", "a.flv"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello" {
		t.Fatalf("content = %q", b)
	}

	if err := s.Delete(ctx, "recordings/demo/a.flv"); err != nil {
		t.Fatal(err)
	}
	objs, _ = s.List(ctx, "")
	if len(objs) != 0 {
		t.Fatalf("after delete = %d, want 0", len(objs))
	}
}

func TestNewDriverSelection(t *testing.T) {
	if _, err := New(Config{Driver: "bogus"}); err == nil {
		t.Fatal("expected an error for an unknown driver")
	}
	if _, err := New(Config{Driver: "s3"}); err == nil {
		t.Fatal("expected an error for s3 without a bucket")
	}
	if _, err := New(Config{Driver: "local", LocalRoot: t.TempDir()}); err != nil {
		t.Fatalf("local driver: %v", err)
	}
}
