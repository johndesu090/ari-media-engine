package srt

import (
	"log/slog"
	"strings"
	"sync"

	gosrt "github.com/datarhei/gosrt"

	"github.com/johndesu090/ari-media-engine/internal/webrtc"
	"github.com/johndesu090/ari-media-engine/pkg/id"
)

// Engine is the subset of the WebRTC engine the SRT ingester needs.
type Engine interface {
	Ingest(streamID, sessionID string) (*webrtc.IngestSession, error)
}

// Hooks receive publish lifecycle notifications.
type Hooks struct {
	OnPublish   func(streamID string)
	OnUnpublish func(streamID string)
	// OnSession is called with the ingest session when a publisher connects.
	OnSession func(streamID string, session *webrtc.IngestSession)
}

// Manager accepts SRT publishers and demuxes their MPEG-TS into the engine.
type Manager struct {
	engine Engine
	log    *slog.Logger
	hooks  Hooks
	addr   string
	cfg    gosrt.Config

	mu     sync.Mutex
	ln     gosrt.Listener
	conns  map[gosrt.Conn]struct{}
	closed bool
}

// NewManager constructs an SRT ingest Manager listening on addr.
func NewManager(engine Engine, addr string, log *slog.Logger, hooks Hooks) *Manager {
	if log == nil {
		log = slog.Default()
	}
	cfg := gosrt.DefaultConfig()
	cfg.Logger = discardLogger{}
	return &Manager{
		engine: engine,
		log:    log,
		hooks:  hooks,
		addr:   addr,
		cfg:    cfg,
		conns:  map[gosrt.Conn]struct{}{},
	}
}

// Start binds the SRT listener and serves publishers.
func (m *Manager) Start() error {
	ln, err := gosrt.Listen("srt", m.addr, m.cfg)
	if err != nil {
		return err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		ln.Close()
		return nil
	}
	m.ln = ln
	m.mu.Unlock()

	go m.acceptLoop(ln)
	m.log.Info("srt listener started", "addr", ln.Addr().String())
	return nil
}

// Addr returns the bound listener address, or "" before Start.
func (m *Manager) Addr() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ln == nil {
		return ""
	}
	return m.ln.Addr().String()
}

// Stop closes the listener and every active connection.
func (m *Manager) Stop() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	ln := m.ln
	conns := make([]gosrt.Conn, 0, len(m.conns))
	for c := range m.conns {
		conns = append(conns, c)
	}
	m.conns = map[gosrt.Conn]struct{}{}
	m.mu.Unlock()

	if ln != nil {
		ln.Close()
	}
	for _, c := range conns {
		c.Close()
	}
}

func (m *Manager) acceptLoop(ln gosrt.Listener) {
	for {
		req, err := ln.Accept2()
		if err != nil {
			return
		}
		go m.handle(req)
	}
}

func (m *Manager) handle(req gosrt.ConnRequest) {
	streamKey := sanitizeStreamKey(req.StreamId())
	if streamKey == "" {
		streamKey = id.New("srt")
	}

	conn, err := req.Accept()
	if err != nil {
		m.log.Warn("srt accept failed", "err", err)
		return
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		conn.Close()
		return
	}
	m.conns[conn] = struct{}{}
	m.mu.Unlock()

	m.log.Info("srt publisher connected", "stream", streamKey, "remote", req.RemoteAddr().String())
	s := &session{streamKey: streamKey, engine: m.engine, hooks: m.hooks, log: m.log.With("stream", streamKey)}
	s.run(conn)

	m.mu.Lock()
	delete(m.conns, conn)
	m.mu.Unlock()
	m.log.Info("srt publisher disconnected", "stream", streamKey)
}

type session struct {
	streamKey string
	engine    Engine
	hooks     Hooks
	log       *slog.Logger
}

func (s *session) run(conn gosrt.Conn) {
	ing, err := s.engine.Ingest(s.streamKey, "srt:"+s.streamKey)
	if err != nil {
		s.log.Warn("srt ingest rejected", "err", err)
		conn.Close()
		return
	}
	if s.hooks.OnPublish != nil {
		s.hooks.OnPublish(s.streamKey)
	}
	if s.hooks.OnSession != nil {
		s.hooks.OnSession(s.streamKey, ing)
	}
	defer func() {
		ing.Close()
		conn.Close()
		if s.hooks.OnUnpublish != nil {
			s.hooks.OnUnpublish(s.streamKey)
		}
	}()

	var configured bool
	var sps, pps []byte
	demux := newTSDemuxer(func(nalus [][]byte, pts uint32, key bool) {
		for _, n := range nalus {
			if len(n) == 0 {
				continue
			}
			switch n[0] & 0x1f {
			case 7:
				sps = n
			case 8:
				pps = n
			}
		}
		if !configured && len(sps) > 0 && len(pps) > 0 {
			if err := ing.SetVideoConfig(sps, pps); err == nil {
				configured = true
			}
		}
		_ = ing.WriteVideo(nalus, pts, key)
	})

	buf := make([]byte, 1500)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			demux.push(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// sanitizeStreamKey turns an SRT stream id into a safe stream key.
func sanitizeStreamKey(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return strings.Trim(string(out), "_")
}

// discardLogger silences gosrt's internal logging.
type discardLogger struct{}

func (discardLogger) HasTopic(string) bool                     { return false }
func (discardLogger) Print(string, uint32, int, func() string) {}
func (discardLogger) Listen() <-chan gosrt.Log                 { return nil }
func (discardLogger) Close()                                   {}
