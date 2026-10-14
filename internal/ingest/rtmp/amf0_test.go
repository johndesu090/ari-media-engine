package rtmp

import (
	"bytes"
	"testing"
)

func TestAMF0RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)

	if err := enc.WriteString("connect"); err != nil {
		t.Fatal(err)
	}
	if err := enc.WriteNumber(1); err != nil {
		t.Fatal(err)
	}
	if err := enc.WriteBool(true); err != nil {
		t.Fatal(err)
	}
	if err := enc.WriteNull(); err != nil {
		t.Fatal(err)
	}
	if err := enc.WriteObject(map[string]any{"app": "live", "port": 1935.0}); err != nil {
		t.Fatal(err)
	}

	dec := NewDecoder(&buf)
	if v, _ := dec.Decode(); v != "connect" {
		t.Fatalf("string = %v", v)
	}
	if v, _ := dec.Decode(); v != 1.0 {
		t.Fatalf("number = %v", v)
	}
	if v, _ := dec.Decode(); v != true {
		t.Fatalf("bool = %v", v)
	}
	if v, _ := dec.Decode(); v != nil {
		t.Fatalf("null = %v", v)
	}
	v, err := dec.Decode()
	if err != nil {
		t.Fatal(err)
	}
	obj, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("object type = %T", v)
	}
	if obj["app"] != "live" || obj["port"] != 1935.0 {
		t.Fatalf("object = %v", obj)
	}
}

func TestAMF0ECMAArray(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	if err := enc.WriteECMAArray(map[string]any{"width": 1920.0, "height": 1080.0}); err != nil {
		t.Fatal(err)
	}

	dec := NewDecoder(&buf)
	v, err := dec.Decode()
	if err != nil {
		t.Fatal(err)
	}
	obj, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("type = %T", v)
	}
	if obj["width"] != 1920.0 || obj["height"] != 1080.0 {
		t.Fatalf("obj = %v", obj)
	}
}
