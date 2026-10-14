package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// nodeKeyPrefix namespaces node records in Redis.
const nodeKeyPrefix = "ari:nodes:"

// RedisRegistry is a Redis-backed node registry. Each node is a JSON value under
// a per-node key with a TTL refreshed by heartbeats, so crashed nodes expire
// automatically.
type RedisRegistry struct {
	client *redis.Client
	ttl    time.Duration
	prefix string
}

// NewRedisRegistry connects to Redis and returns a registry whose node records
// expire after ttl (default 15s).
func NewRedisRegistry(redisURL string, ttl time.Duration) (*RedisRegistry, error) {
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("cluster: parse redis url: %w", err)
	}
	if ttl <= 0 {
		ttl = 15 * time.Second
	}
	return &RedisRegistry{client: redis.NewClient(opt), ttl: ttl, prefix: nodeKeyPrefix}, nil
}

// Ping verifies connectivity to Redis.
func (r *RedisRegistry) Ping(ctx context.Context) error { return r.client.Ping(ctx).Err() }

// Register writes the node record with a fresh TTL.
func (r *RedisRegistry) Register(ctx context.Context, node Node) error { return r.put(ctx, node) }

// Heartbeat refreshes the node record and its TTL.
func (r *RedisRegistry) Heartbeat(ctx context.Context, node Node) error { return r.put(ctx, node) }

func (r *RedisRegistry) put(ctx context.Context, node Node) error {
	node.LastHeartbeat = time.Now().UTC()
	b, err := json.Marshal(node)
	if err != nil {
		return err
	}
	return r.client.Set(ctx, r.prefix+node.ID, b, r.ttl).Err()
}

// List returns every live node, sorted by id.
func (r *RedisRegistry) List(ctx context.Context) ([]Node, error) {
	var out []Node
	iter := r.client.Scan(ctx, 0, r.prefix+"*", 100).Iterator()
	for iter.Next(ctx) {
		val, err := r.client.Get(ctx, iter.Val()).Bytes()
		if err != nil {
			if errors.Is(err, redis.Nil) {
				continue
			}
			return nil, err
		}
		var n Node
		if err := json.Unmarshal(val, &n); err != nil {
			continue
		}
		out = append(out, n)
	}
	if err := iter.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Deregister removes the node record.
func (r *RedisRegistry) Deregister(ctx context.Context, id string) error {
	return r.client.Del(ctx, r.prefix+id).Err()
}

// Close closes the Redis client.
func (r *RedisRegistry) Close() error { return r.client.Close() }

// MemoryRegistry is an in-process Registry for single-node development and
// tests. Entries expire after ttl like the Redis implementation.
type MemoryRegistry struct {
	mu    sync.Mutex
	nodes map[string]memEntry
	ttl   time.Duration
}

type memEntry struct {
	node    Node
	expires time.Time
}

// NewMemoryRegistry returns an in-memory registry with the given TTL.
func NewMemoryRegistry(ttl time.Duration) *MemoryRegistry {
	if ttl <= 0 {
		ttl = 15 * time.Second
	}
	return &MemoryRegistry{nodes: map[string]memEntry{}, ttl: ttl}
}

// Register stores the node.
func (m *MemoryRegistry) Register(_ context.Context, node Node) error { return m.put(node) }

// Heartbeat refreshes the node and its expiry.
func (m *MemoryRegistry) Heartbeat(_ context.Context, node Node) error { return m.put(node) }

func (m *MemoryRegistry) put(node Node) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	node.LastHeartbeat = time.Now().UTC()
	m.nodes[node.ID] = memEntry{node: node, expires: time.Now().Add(m.ttl)}
	return nil
}

// List returns every live node, sorted by id.
func (m *MemoryRegistry) List(_ context.Context) ([]Node, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	out := make([]Node, 0, len(m.nodes))
	for id, e := range m.nodes {
		if now.After(e.expires) {
			delete(m.nodes, id)
			continue
		}
		out = append(out, e.node)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Deregister removes the node.
func (m *MemoryRegistry) Deregister(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.nodes, id)
	return nil
}

// Close is a no-op.
func (m *MemoryRegistry) Close() error { return nil }
