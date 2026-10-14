// Package ingest manages the non-WebRTC ingestion listeners (RTMP, SRT, RTSP).
//
// This revision provides the listener lifecycle and a pluggable Handler
// interface. Media demuxing and repacketization into the WebRTC track pipeline
// are tracked as follow-up work in docs/architecture.md; until a protocol
// handler is registered the manager accepts and closes connections so that
// port allocation, metrics and graceful shutdown are already correct.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

// Protocol identifies an ingest transport.
type Protocol string

// Supported ingest protocols.
const (
	ProtocolRTMP Protocol = "rtmp"
	ProtocolSRT  Protocol = "srt"
	ProtocolRTSP Protocol = "rtsp"
)

// Handler consumes a single accepted ingest connection.
type Handler interface {
	ServeConn(ctx context.Context, conn net.Conn, protocol Protocol)
}

// HandlerFunc adapts a function to the Handler interface.
type HandlerFunc func(ctx context.Context, conn net.Conn, protocol Protocol)

// ServeConn implements Handler.
func (f HandlerFunc) ServeConn(ctx context.Context, conn net.Conn, protocol Protocol) {
	f(ctx, conn, protocol)
}

// Listener pairs a protocol with its TCP listener and handler.
type Listener struct {
	Protocol Protocol
	Addr     string
	Handler  Handler
}

// Manager owns the ingest listeners for the process.
type Manager struct {
	log *slog.Logger

	mu        sync.Mutex
	listeners []*boundListener
}

type boundListener struct {
	protocol Protocol
	ln       net.Listener
	handler  Handler
}

// New constructs an ingest Manager.
func New(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{log: log}
}

// Start binds every configured listener and serves connections until ctx is
// cancelled or Close is called.
func (m *Manager) Start(ctx context.Context, specs []Listener) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.listeners) > 0 {
		return errors.New("ingest: manager already started")
	}

	bound := make([]*boundListener, 0, len(specs))
	for _, spec := range specs {
		ln, err := net.Listen("tcp", spec.Addr)
		if err != nil {
			for _, b := range bound {
				_ = b.ln.Close()
			}
			return fmt.Errorf("ingest: listen %s %s: %w", spec.Protocol, spec.Addr, err)
		}
		handler := spec.Handler
		if handler == nil {
			handler = logAndCloseHandler(m.log)
		}
		b := &boundListener{protocol: spec.Protocol, ln: ln, handler: handler}
		bound = append(bound, b)
		m.log.Info("ingest listener started", "protocol", spec.Protocol, "addr", ln.Addr().String())
	}
	m.listeners = bound

	for _, b := range bound {
		go m.acceptLoop(ctx, b)
	}
	return nil
}

// Close stops every listener.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var firstErr error
	for _, b := range m.listeners {
		if err := b.ln.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	m.listeners = nil
	return firstErr
}

// Addrs returns the bound protocol addresses, useful for logging and tests.
func (m *Manager) Addrs() map[Protocol]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[Protocol]string, len(m.listeners))
	for _, b := range m.listeners {
		out[b.protocol] = b.ln.Addr().String()
	}
	return out
}

func (m *Manager) acceptLoop(ctx context.Context, b *boundListener) {
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			m.log.Warn("ingest accept error", "protocol", b.protocol, "err", err)
			continue
		}
		go func() {
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
			b.handler.ServeConn(ctx, conn, b.protocol)
		}()
	}
}

// logAndCloseHandler is the default handler used until a real protocol
// implementation is registered.
func logAndCloseHandler(log *slog.Logger) Handler {
	return HandlerFunc(func(_ context.Context, conn net.Conn, protocol Protocol) {
		log.Info("ingest connection received (no handler registered)",
			"protocol", protocol, "remote", conn.RemoteAddr().String())
	})
}
