package broadcast

import (
	"context"
	"errors"
	"sort"
	"sync"
)

// Sentinel errors returned by Store implementations.
var (
	// ErrNotFound indicates no broadcast matches the lookup.
	ErrNotFound = errors.New("broadcast: not found")
	// ErrExists indicates a broadcast with the same identity already exists.
	ErrExists = errors.New("broadcast: already exists")
)

// Store persists broadcasts. The interface is intentionally small so that a
// Redis- or database-backed implementation can be swapped in for clustering.
type Store interface {
	Create(ctx context.Context, b *Broadcast) error
	Get(ctx context.Context, id string) (*Broadcast, error)
	GetByStreamID(ctx context.Context, streamID string) (*Broadcast, error)
	List(ctx context.Context) ([]*Broadcast, error)
	Delete(ctx context.Context, id string) error
}

// MemoryStore is an in-process Store implementation suitable for a single node
// and for development. It is safe for concurrent use.
type MemoryStore struct {
	mu         sync.RWMutex
	byID       map[string]*Broadcast
	byStreamID map[string]*Broadcast
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		byID:       make(map[string]*Broadcast),
		byStreamID: make(map[string]*Broadcast),
	}
}

// Create stores a new broadcast.
func (s *MemoryStore) Create(_ context.Context, b *Broadcast) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[b.ID()]; ok {
		return ErrExists
	}
	if _, ok := s.byStreamID[b.StreamID()]; ok {
		return ErrExists
	}
	s.byID[b.ID()] = b
	s.byStreamID[b.StreamID()] = b
	return nil
}

// Get returns the broadcast with the given id.
func (s *MemoryStore) Get(_ context.Context, id string) (*Broadcast, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.byID[id]
	if !ok {
		return nil, ErrNotFound
	}
	return b, nil
}

// GetByStreamID returns the broadcast with the given stream key.
func (s *MemoryStore) GetByStreamID(_ context.Context, streamID string) (*Broadcast, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.byStreamID[streamID]
	if !ok {
		return nil, ErrNotFound
	}
	return b, nil
}

// List returns all broadcasts sorted by creation time (newest first).
func (s *MemoryStore) List(_ context.Context) ([]*Broadcast, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Broadcast, 0, len(s.byID))
	for _, b := range s.byID {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Snapshot().CreatedAt.After(out[j].Snapshot().CreatedAt)
	})
	return out, nil
}

// Delete removes a broadcast.
func (s *MemoryStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.byID[id]
	if !ok {
		return ErrNotFound
	}
	delete(s.byID, id)
	delete(s.byStreamID, b.StreamID())
	return nil
}
