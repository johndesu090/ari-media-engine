// Package rtsp implements RTSP pull ingest: it connects to an RTSP source
// (e.g. an IP camera), performs OPTIONS/DESCRIBE/SETUP/PLAY over a single TCP
// connection, depacketizes the interleaved H.264 RTP, and feeds the WebRTC
// engine's ingest session so subscribers receive the stream.
package rtsp

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/pion/rtp"

	"github.com/johndesu090/ari-media-engine/internal/webrtc"
)

// Engine is the subset of the WebRTC engine the RTSP puller needs.
type Engine interface {
	Ingest(streamID, sessionID string) (*webrtc.IngestSession, error)
}

// Hooks receive pull lifecycle notifications.
type Hooks struct {
	OnPublish   func(streamID string)
	OnUnpublish func(streamID string)
	// OnSession is called with the ingest session each time a pull (re)connects.
	OnSession func(streamID string, session *webrtc.IngestSession)
}

// Manager manages RTSP pull sessions keyed by stream id.
type Manager struct {
	engine Engine
	log    *slog.Logger
	hooks  Hooks

	mu       sync.Mutex
	sessions map[string]*session
}

// NewManager constructs an RTSP pull Manager.
func NewManager(engine Engine, log *slog.Logger, hooks Hooks) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{engine: engine, log: log, hooks: hooks, sessions: map[string]*session{}}
}

// PullInfo describes an RTSP pull session.
type PullInfo struct {
	StreamID string `json:"streamId"`
	URL      string `json:"url"`
	State    string `json:"state"`
	Error    string `json:"error,omitempty"`
}

// Start begins pulling rawURL into streamID.
func (m *Manager) Start(streamID, rawURL string) error {
	if streamID == "" || rawURL == "" {
		return errors.New("rtsp: streamId and url are required")
	}
	m.mu.Lock()
	if _, ok := m.sessions[streamID]; ok {
		m.mu.Unlock()
		return errors.New("rtsp: already pulling stream " + streamID)
	}
	s := &session{
		streamID: streamID,
		url:      rawURL,
		engine:   m.engine,
		hooks:    m.hooks,
		log:      m.log.With("stream", streamID),
		stop:     make(chan struct{}),
		state:    "connecting",
	}
	m.sessions[streamID] = s
	m.mu.Unlock()

	go s.run()
	m.log.Info("rtsp pull started", "stream", streamID, "url", rawURL)
	return nil
}

// Stop ends the pull session for streamID.
func (m *Manager) Stop(streamID string) {
	m.mu.Lock()
	s, ok := m.sessions[streamID]
	if ok {
		delete(m.sessions, streamID)
	}
	m.mu.Unlock()
	if ok {
		s.close()
	}
}

// StopAll ends every pull session.
func (m *Manager) StopAll() {
	m.mu.Lock()
	sessions := m.sessions
	m.sessions = map[string]*session{}
	m.mu.Unlock()
	for _, s := range sessions {
		s.close()
	}
}

// List returns the current pull sessions.
func (m *Manager) List() []PullInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]PullInfo, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s.info())
	}
	return out
}

type session struct {
	streamID string
	url      string
	engine   Engine
	hooks    Hooks
	log      *slog.Logger
	stop     chan struct{}

	mu        sync.Mutex
	closed    bool
	published bool
	state     string
	lastErr   string
}

func (s *session) info() PullInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return PullInfo{StreamID: s.streamID, URL: s.url, State: s.state, Error: s.lastErr}
}

func (s *session) setState(state, errMsg string) {
	s.mu.Lock()
	s.state = state
	s.lastErr = errMsg
	s.mu.Unlock()
}

func (s *session) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	published := s.published
	close(s.stop)
	s.mu.Unlock()

	if published && s.hooks.OnUnpublish != nil {
		s.hooks.OnUnpublish(s.streamID)
	}
	s.log.Info("rtsp pull stopped", "stream", s.streamID)
}

func (s *session) stopped() bool {
	select {
	case <-s.stop:
		return true
	default:
		return false
	}
}

func (s *session) run() {
	for {
		if s.stopped() {
			return
		}
		s.setState("connecting", "")

		ing, err := s.engine.Ingest(s.streamID, "rtsp:"+s.streamID)
		if err != nil {
			s.setState("error", err.Error())
			if !s.wait() {
				return
			}
			continue
		}

		s.mu.Lock()
		first := !s.published
		s.published = true
		s.mu.Unlock()
		if first && s.hooks.OnPublish != nil {
			s.hooks.OnPublish(s.streamID)
		}
		if s.hooks.OnSession != nil {
			s.hooks.OnSession(s.streamID, ing)
		}

		s.setState("running", "")
		err = s.pull(ing)
		ing.Close()

		if err != nil {
			s.setState("error", err.Error())
			s.log.Warn("rtsp pull error", "stream", s.streamID, "err", err)
		} else {
			s.setState("stopped", "")
		}
		if !s.wait() {
			return
		}
	}
}

// wait backs off before reconnecting; it returns false when the session stopped.
func (s *session) wait() bool {
	select {
	case <-s.stop:
		return false
	case <-time.After(3 * time.Second):
		return true
	}
}

func (s *session) pull(ing *webrtc.IngestSession) error {
	c, err := dialRTSP(s.url, 10*time.Second)
	if err != nil {
		return err
	}
	defer c.close()

	desc, err := c.setup(s.url)
	if err != nil {
		return err
	}

	sps, pps := desc.sps, desc.pps
	configured := false
	if len(sps) > 0 && len(pps) > 0 {
		if err := ing.SetVideoConfig(sps, pps); err != nil {
			return err
		}
		configured = true
	}

	kaDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-kaDone:
				return
			case <-s.stop:
				return
			case <-ticker.C:
				_ = c.keepAlive(s.url)
			}
		}
	}()
	defer close(kaDone)

	depack := &h264Depacketizer{}
	var auNalus [][]byte
	auKey := false

	return c.readLoop(func(payload []byte) {
		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(payload); err != nil {
			return
		}
		nalus, key := depack.push(pkt.Payload)

		for _, nalu := range nalus {
			if len(nalu) == 0 {
				continue
			}
			switch nalu[0] & 0x1f {
			case 7:
				sps = nalu
			case 8:
				pps = nalu
			}
		}
		if !configured && len(sps) > 0 && len(pps) > 0 {
			if err := ing.SetVideoConfig(sps, pps); err == nil {
				configured = true
			}
		}

		if len(nalus) > 0 {
			auNalus = append(auNalus, nalus...)
		}
		if key {
			auKey = true
		}
		if pkt.Marker && len(auNalus) > 0 {
			_ = ing.WriteVideo(auNalus, pkt.Timestamp/90, auKey)
			auNalus = nil
			auKey = false
		}
	})
}
