// Package webrtc implements the Ari Media Engine WebRTC SFU. A single publisher
// pushes media into a stream and every subscriber receives the same RTP packets
// fanned out through TrackLocalStaticRTP, delivering sub-second glass-to-glass
// latency.
//
// This is a deliberately small, correct SFU skeleton:
//   - one publisher per stream, many subscribers
//   - non-trickle ICE (the full SDP is exchanged in a single message)
//   - RTP forwarding without transcoding
//
// Renegotiation, simulcast/SVC, trickle ICE and congestion control are tracked
// as follow-up work in docs/architecture.md.
package webrtc

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/rtp"
	pion "github.com/pion/webrtc/v4"
)

// Sentinel errors surfaced to the signaling layer and REST API.
var (
	// ErrBroadcastNotFound indicates the requested stream does not exist.
	ErrBroadcastNotFound = errors.New("webrtc: broadcast not found")
	// ErrBroadcastLive indicates the stream already has a publisher.
	ErrBroadcastLive = errors.New("webrtc: broadcast already has a publisher")
	// ErrNotLive indicates the stream is not yet publishing media.
	ErrNotLive = errors.New("webrtc: broadcast is not live")
	// ErrSessionNotFound indicates the subscriber session is unknown.
	ErrSessionNotFound = errors.New("webrtc: session not found")
	// ErrMaxSubscribers indicates the per-stream subscriber cap was reached.
	ErrMaxSubscribers = errors.New("webrtc: max subscribers reached")
)

const (
	gatherTimeout = 5 * time.Second
	// liveDebounce waits briefly after the first track arrives so that all
	// tracks (audio + video) are collected before the stream is marked live.
	liveDebounce = 200 * time.Millisecond
)

// Config configures the WebRTC engine.
type Config struct {
	// ICEPublicIPs are advertised as host candidates for NAT traversal.
	ICEPublicIPs []string
	// ICETCPPort enables ICE-TCP on the given port when non-zero.
	ICETCPPort int
	// ICEUDPPortMin/Max bound the ephemeral UDP port range for media.
	ICEUDPPortMin int
	ICEUDPPortMax int
	// EnableLoopback includes loopback candidates (useful for local dev).
	EnableLoopback bool
	// MaxSubscribers caps subscribers per stream (0 = unlimited).
	MaxSubscribers int
}

// Engine owns every stream and PeerConnection in the process.
type Engine struct {
	log *slog.Logger
	api *pion.API
	cfg Config

	// tcpMux is the ICE-TCP multiplexer, closed on Shutdown.
	tcpMux io.Closer

	mu      sync.RWMutex
	streams map[string]*stream
}

// NewEngine constructs an Engine from cfg.
func NewEngine(cfg Config, log *slog.Logger) (*Engine, error) {
	if log == nil {
		log = slog.Default()
	}

	var settings pion.SettingEngine
	if cfg.ICEUDPPortMin > 0 && cfg.ICEUDPPortMax > 0 {
		if err := settings.SetEphemeralUDPPortRange(uint16(cfg.ICEUDPPortMin), uint16(cfg.ICEUDPPortMax)); err != nil {
			return nil, err
		}
	}

	var tcpMux io.Closer
	if cfg.ICETCPPort > 0 {
		ln, err := net.ListenTCP("tcp", &net.TCPAddr{Port: cfg.ICETCPPort})
		if err != nil {
			return nil, fmt.Errorf("webrtc: ICE-TCP listen on port %d: %w", cfg.ICETCPPort, err)
		}
		mux := ice.NewTCPMuxDefault(ice.TCPMuxParams{Listener: ln})
		settings.SetICETCPMux(mux)
		tcpMux = mux
	}

	if len(cfg.ICEPublicIPs) > 0 {
		settings.SetNAT1To1IPs(cfg.ICEPublicIPs, pion.ICECandidateTypeHost)
	}
	settings.SetIncludeLoopbackCandidate(cfg.EnableLoopback)

	media := &pion.MediaEngine{}
	if err := media.RegisterDefaultCodecs(); err != nil {
		if tcpMux != nil {
			_ = tcpMux.Close()
		}
		return nil, err
	}

	api := pion.NewAPI(
		pion.WithMediaEngine(media),
		pion.WithSettingEngine(settings),
	)

	return &Engine{
		log:     log,
		api:     api,
		cfg:     cfg,
		tcpMux:  tcpMux,
		streams: make(map[string]*stream),
	}, nil
}

// stream is the per-broadcast SFU state.
type stream struct {
	id  string
	log *slog.Logger

	mu          sync.RWMutex
	publisher   *peer
	subscribers map[string]*peer
	tracks      []*pion.TrackLocalStaticRTP
	live        bool
	liveTimer   *time.Timer
}

// Publish attaches a publisher to streamID and returns the SDP answer.
func (e *Engine) Publish(streamID, sessionID, offerSDP string) (string, error) {
	s := e.getOrCreateStream(streamID)

	s.mu.Lock()
	if s.publisher != nil {
		s.mu.Unlock()
		return "", ErrBroadcastLive
	}
	s.mu.Unlock()

	pc, err := e.api.NewPeerConnection(pion.Configuration{})
	if err != nil {
		return "", err
	}

	pc.OnTrack(func(remote *pion.TrackRemote, _ *pion.RTPReceiver) {
		local, err := pion.NewTrackLocalStaticRTP(remote.Codec().RTPCodecCapability, remote.ID(), streamID)
		if err != nil {
			s.log.Error("create local track", "err", err)
			return
		}
		s.addTrack(local)
		go s.forward(remote, local)
		s.scheduleLive()
		s.log.Info("publisher track", "kind", remote.Kind().String(), "codec", remote.Codec().MimeType)
	})

	pc.OnConnectionStateChange(func(state pion.PeerConnectionState) {
		switch state {
		case pion.PeerConnectionStateFailed,
			pion.PeerConnectionStateClosed,
			pion.PeerConnectionStateDisconnected:
			e.removePublisher(streamID, sessionID)
		}
	})

	if err := pc.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeOffer, SDP: offerSDP}); err != nil {
		_ = pc.Close()
		return "", err
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		_ = pc.Close()
		return "", err
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		_ = pc.Close()
		return "", err
	}
	if err := waitGathering(pc); err != nil {
		_ = pc.Close()
		return "", err
	}

	s.mu.Lock()
	if s.publisher != nil {
		s.mu.Unlock()
		_ = pc.Close()
		return "", ErrBroadcastLive
	}
	s.publisher = &peer{id: sessionID, role: rolePublisher, pc: pc}
	s.mu.Unlock()

	local := pc.LocalDescription()
	if local == nil {
		return "", errors.New("webrtc: nil local description after gathering")
	}
	s.log.Info("publisher connected", "session", sessionID)
	return local.SDP, nil
}

// Subscribe creates a subscriber PeerConnection and returns the SDP offer.
func (e *Engine) Subscribe(streamID, sessionID string) (string, error) {
	e.mu.RLock()
	s, ok := e.streams[streamID]
	e.mu.RUnlock()
	if !ok {
		return "", ErrBroadcastNotFound
	}
	if !s.isLive() {
		return "", ErrNotLive
	}

	tracks := s.trackSnapshot()
	if len(tracks) == 0 {
		return "", ErrNotLive
	}

	pc, err := e.api.NewPeerConnection(pion.Configuration{})
	if err != nil {
		return "", err
	}

	for _, t := range tracks {
		if _, err := pc.AddTrack(t); err != nil {
			_ = pc.Close()
			return "", err
		}
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		_ = pc.Close()
		return "", err
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		_ = pc.Close()
		return "", err
	}
	if err := waitGathering(pc); err != nil {
		_ = pc.Close()
		return "", err
	}

	pc.OnConnectionStateChange(func(state pion.PeerConnectionState) {
		switch state {
		case pion.PeerConnectionStateFailed,
			pion.PeerConnectionStateClosed,
			pion.PeerConnectionStateDisconnected:
			s.removeSubscriber(sessionID)
		}
	})

	if err := s.addSubscriber(sessionID, &peer{id: sessionID, role: roleSubscriber, pc: pc}, e.cfg.MaxSubscribers); err != nil {
		_ = pc.Close()
		return "", err
	}

	local := pc.LocalDescription()
	if local == nil {
		return "", errors.New("webrtc: nil local description after gathering")
	}
	return local.SDP, nil
}

// SetSubscriberAnswer applies the subscriber's SDP answer.
func (e *Engine) SetSubscriberAnswer(streamID, sessionID, answerSDP string) error {
	e.mu.RLock()
	s, ok := e.streams[streamID]
	e.mu.RUnlock()
	if !ok {
		return ErrBroadcastNotFound
	}
	p := s.getSubscriber(sessionID)
	if p == nil {
		return ErrSessionNotFound
	}
	return p.pc.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeAnswer, SDP: answerSDP})
}

// CloseSession removes whichever role sessionID held on streamID.
func (e *Engine) CloseSession(streamID, sessionID string) {
	e.mu.RLock()
	s, ok := e.streams[streamID]
	e.mu.RUnlock()
	if !ok {
		return
	}
	s.removeSubscriber(sessionID)
	e.removePublisher(streamID, sessionID)
}

// HasLiveStream reports whether streamID currently has media flowing.
func (e *Engine) HasLiveStream(streamID string) bool {
	e.mu.RLock()
	s, ok := e.streams[streamID]
	e.mu.RUnlock()
	return ok && s.isLive()
}

// ViewerCount returns the number of subscribers on streamID.
func (e *Engine) ViewerCount(streamID string) int {
	e.mu.RLock()
	s, ok := e.streams[streamID]
	e.mu.RUnlock()
	if !ok {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.subscribers)
}

// StreamCount returns the number of tracked streams.
func (e *Engine) StreamCount() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.streams)
}

// Shutdown closes every PeerConnection and stream.
func (e *Engine) Shutdown() {
	e.mu.Lock()
	streams := e.streams
	e.streams = make(map[string]*stream)
	e.mu.Unlock()

	for _, s := range streams {
		s.mu.Lock()
		pub := s.publisher
		subs := make([]*peer, 0, len(s.subscribers))
		for _, sp := range s.subscribers {
			subs = append(subs, sp)
		}
		s.publisher = nil
		s.subscribers = make(map[string]*peer)
		s.mu.Unlock()

		if pub != nil {
			pub.close()
		}
		for _, sp := range subs {
			sp.close()
		}
	}

	if e.tcpMux != nil {
		_ = e.tcpMux.Close()
	}
}

func (e *Engine) getOrCreateStream(id string) *stream {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.streams[id]
	if !ok {
		s = &stream{
			id:          id,
			log:         e.log.With("stream", id),
			subscribers: make(map[string]*peer),
		}
		e.streams[id] = s
	}
	return s
}

func (e *Engine) removePublisher(streamID, sessionID string) {
	e.mu.RLock()
	s, ok := e.streams[streamID]
	e.mu.RUnlock()
	if !ok {
		return
	}

	s.mu.Lock()
	if s.publisher == nil || s.publisher.id != sessionID {
		s.mu.Unlock()
		return
	}
	pub := s.publisher
	subs := make([]*peer, 0, len(s.subscribers))
	for _, sp := range s.subscribers {
		subs = append(subs, sp)
	}
	s.publisher = nil
	s.subscribers = make(map[string]*peer)
	s.tracks = nil
	s.live = false
	if s.liveTimer != nil {
		s.liveTimer.Stop()
		s.liveTimer = nil
	}
	s.mu.Unlock()

	pub.close()
	for _, sp := range subs {
		sp.close()
	}
	s.log.Info("publisher removed; stream reset", "session", sessionID, "subscribers", len(subs))
}

func (s *stream) isLive() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.live
}

func (s *stream) trackSnapshot() []*pion.TrackLocalStaticRTP {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*pion.TrackLocalStaticRTP, len(s.tracks))
	copy(out, s.tracks)
	return out
}

func (s *stream) addTrack(t *pion.TrackLocalStaticRTP) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tracks = append(s.tracks, t)
}

func (s *stream) addSubscriber(sessionID string, p *peer, max int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if max > 0 && len(s.subscribers) >= max {
		return ErrMaxSubscribers
	}
	s.subscribers[sessionID] = p
	return nil
}

func (s *stream) getSubscriber(sessionID string) *peer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.subscribers[sessionID]
}

func (s *stream) removeSubscriber(sessionID string) {
	s.mu.Lock()
	p, ok := s.subscribers[sessionID]
	if ok {
		delete(s.subscribers, sessionID)
	}
	s.mu.Unlock()
	if ok {
		p.close()
	}
}

func (s *stream) scheduleLive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live {
		return
	}
	if s.liveTimer != nil {
		s.liveTimer.Stop()
	}
	s.liveTimer = time.AfterFunc(liveDebounce, func() {
		s.mu.Lock()
		s.live = true
		s.liveTimer = nil
		s.mu.Unlock()
		s.log.Info("stream live")
	})
}

// forward copies RTP packets from the publisher track to the local fan-out track.
func (s *stream) forward(remote *pion.TrackRemote, local *pion.TrackLocalStaticRTP) {
	buf := make([]byte, 1500)
	for {
		n, _, err := remote.Read(buf)
		if err != nil {
			return
		}
		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}
		if err := local.WriteRTP(pkt); err != nil {
			return
		}
	}
}

func waitGathering(pc *pion.PeerConnection) error {
	select {
	case <-pion.GatheringCompletePromise(pc):
		return nil
	case <-time.After(gatherTimeout):
		return errors.New("webrtc: ICE gathering timed out")
	}
}
