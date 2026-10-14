// Package cluster provides Redis-backed node discovery and heartbeat for
// horizontal scaling. Each node registers itself with a TTL-refreshed heartbeat
// and can enumerate the rest of the cluster.
package cluster

import (
	"context"
	"time"
)

// Node describes one engine node participating in a cluster.
type Node struct {
	ID            string    `json:"id"`
	Address       string    `json:"address"`
	Version       string    `json:"version"`
	StartedAt     time.Time `json:"startedAt"`
	LastHeartbeat time.Time `json:"lastHeartbeat"`
	Streams       int       `json:"streams"`
	Viewers       int       `json:"viewers"`
}

// Registry tracks the nodes participating in a cluster.
type Registry interface {
	Register(ctx context.Context, node Node) error
	Heartbeat(ctx context.Context, node Node) error
	List(ctx context.Context) ([]Node, error)
	Deregister(ctx context.Context, id string) error
	Close() error
}
