package cluster

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func TestRedisRegistry(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()

	reg, err := NewRedisRegistry("redis://"+mr.Addr(), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	ctx := context.Background()

	if err := reg.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := reg.Register(ctx, Node{ID: "n1", Address: "1.2.3.4:8080", Version: "test", Streams: 2, Viewers: 5}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(ctx, Node{ID: "n2", Address: "5.6.7.8:8080", Version: "test"}); err != nil {
		t.Fatal(err)
	}

	nodes, err := reg.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(nodes))
	}
	if nodes[0].ID != "n1" || nodes[1].ID != "n2" {
		t.Fatalf("order = %s, %s", nodes[0].ID, nodes[1].ID)
	}
	if nodes[0].Streams != 2 || nodes[0].Viewers != 5 {
		t.Fatalf("stats = %+v", nodes[0])
	}
	if nodes[0].LastHeartbeat.IsZero() {
		t.Fatal("expected a heartbeat timestamp")
	}

	if err := reg.Deregister(ctx, "n1"); err != nil {
		t.Fatal(err)
	}
	nodes, _ = reg.List(ctx)
	if len(nodes) != 1 || nodes[0].ID != "n2" {
		t.Fatalf("after deregister = %+v", nodes)
	}
}

func TestRedisRegistryTTLExpiry(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Close()

	reg, err := NewRedisRegistry("redis://"+mr.Addr(), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	ctx := context.Background()

	if err := reg.Register(ctx, Node{ID: "n1"}); err != nil {
		t.Fatal(err)
	}
	mr.FastForward(11 * time.Second)

	nodes, err := reg.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 0 {
		t.Fatalf("expected the node to expire, got %+v", nodes)
	}
}

func TestMemoryRegistry(t *testing.T) {
	reg := NewMemoryRegistry(time.Minute)
	ctx := context.Background()

	_ = reg.Register(ctx, Node{ID: "a"})
	_ = reg.Register(ctx, Node{ID: "b"})
	nodes, _ := reg.List(ctx)
	if len(nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(nodes))
	}
	_ = reg.Deregister(ctx, "a")
	nodes, _ = reg.List(ctx)
	if len(nodes) != 1 || nodes[0].ID != "b" {
		t.Fatalf("after deregister = %+v", nodes)
	}
}

func TestManagerHeartbeatAndStop(t *testing.T) {
	reg := NewMemoryRegistry(time.Minute)
	m := NewManager(Options{
		Registry:  reg,
		NodeID:    "node-a",
		Address:   "host:8080",
		Version:   "test",
		Heartbeat: 50 * time.Millisecond,
		Stats:     func() (int, int) { return 3, 7 },
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)

	nodes, err := m.Nodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(nodes))
	}
	if nodes[0].ID != "node-a" || nodes[0].Streams != 3 || nodes[0].Viewers != 7 {
		t.Fatalf("node = %+v", nodes[0])
	}

	m.Stop(context.Background())
	nodes, _ = m.Nodes(context.Background())
	if len(nodes) != 0 {
		t.Fatalf("after stop = %d, want 0", len(nodes))
	}
}
