package cluster

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Stats reports the local node's stream and viewer counts for heartbeats.
type Stats func() (streams, viewers int)

// Manager registers the local node, refreshes its heartbeat and exposes cluster
// membership.
type Manager struct {
	registry Registry
	id       string
	address  string
	version  string
	interval time.Duration
	started  time.Time
	log      *slog.Logger
	stats    Stats

	streams atomic.Int64
	viewers atomic.Int64

	mu      sync.Mutex
	cancel  context.CancelFunc
	stopped bool
}

// Options configures a Manager.
type Options struct {
	Registry  Registry
	NodeID    string
	Address   string
	Version   string
	Heartbeat time.Duration
	Logger    *slog.Logger
	Stats     Stats
}

// NewManager constructs a Manager. A nil logger defaults to slog.Default.
func NewManager(opts Options) *Manager {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	interval := opts.Heartbeat
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &Manager{
		registry: opts.Registry,
		id:       opts.NodeID,
		address:  opts.Address,
		version:  opts.Version,
		interval: interval,
		started:  time.Now().UTC(),
		log:      log,
		stats:    opts.Stats,
	}
}

// Start registers the node and begins heartbeating until ctx is cancelled or
// Stop is called.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.mu.Unlock()

	m.refresh()
	if err := m.registry.Register(ctx, m.node()); err != nil {
		return err
	}
	go m.loop(ctx)
	m.log.Info("cluster node registered", "node", m.id, "address", m.address, "heartbeat", m.interval.String())
	return nil
}

// Stop deregisters the node and stops heartbeating.
func (m *Manager) Stop(ctx context.Context) {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	m.stopped = true
	cancel := m.cancel
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if err := m.registry.Deregister(ctx, m.id); err != nil {
		m.log.Warn("cluster deregister failed", "err", err)
	}
	m.log.Info("cluster node deregistered", "node", m.id)
}

// NodeID returns the local node identifier.
func (m *Manager) NodeID() string { return m.id }

// Nodes returns the current cluster membership.
func (m *Manager) Nodes(ctx context.Context) ([]Node, error) { return m.registry.List(ctx) }

// Close stops heartbeating, deregisters the node and releases the registry.
func (m *Manager) Close(ctx context.Context) {
	m.Stop(ctx)
	_ = m.registry.Close()
}

func (m *Manager) loop(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.refresh()
			if err := m.registry.Heartbeat(ctx, m.node()); err != nil {
				m.log.Warn("cluster heartbeat failed", "err", err)
			}
		}
	}
}

func (m *Manager) refresh() {
	if m.stats == nil {
		return
	}
	streams, viewers := m.stats()
	m.streams.Store(int64(streams))
	m.viewers.Store(int64(viewers))
}

func (m *Manager) node() Node {
	return Node{
		ID:            m.id,
		Address:       m.address,
		Version:       m.version,
		StartedAt:     m.started,
		LastHeartbeat: time.Now().UTC(),
		Streams:       int(m.streams.Load()),
		Viewers:       int(m.viewers.Load()),
	}
}
