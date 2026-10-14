// Package p2p implements the Ari P2P engine's server side: a tracker that groups
// viewers of a stream into a channel and relays WebRTC signaling so peers can
// form data-channel meshes and share HLS/DASH segments with CDN fallback.
//
// The tracker is deliberately transport-agnostic about media: it never sees the
// segment bytes. It records which segments each peer holds (`have`), answers
// `want` queries with candidate peers, and relays `signal` messages between
// peers so they can open RTCDataChannels and transfer segments directly.
package p2p

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/johndesu090/ari-media-engine/pkg/id"
)

// Message types exchanged with peers.
const (
	TypeJoin       = "join"
	TypeLeave      = "leave"
	TypeSignal     = "signal"
	TypeHave       = "have"
	TypeWant       = "want"
	TypeJoined     = "joined"
	TypePeerJoined = "peer-joined"
	TypePeerLeft   = "peer-left"
	TypePeers      = "peers"
)

// Message is the JSON envelope for the tracker protocol.
type Message struct {
	Type          string   `json:"type"`
	Channel       string   `json:"channel,omitempty"`
	PeerID        string   `json:"peerId,omitempty"`
	To            string   `json:"to,omitempty"`
	From          string   `json:"from,omitempty"`
	SDP           string   `json:"sdp,omitempty"`
	SDPType       string   `json:"sdpType,omitempty"`
	Candidate     string   `json:"candidate,omitempty"`
	SDPMid        *string  `json:"sdpMid,omitempty"`
	SDPMLineIndex *uint16  `json:"sdpMLineIndex,omitempty"`
	Segments      []string `json:"segments,omitempty"`
	Segment       string   `json:"segment,omitempty"`
	Peers         []string `json:"peers,omitempty"`
}

// Hub groups peers into channels.
type Hub struct {
	log      *slog.Logger
	maxPeers int
	upgrader websocket.Upgrader

	mu       sync.Mutex
	channels map[string]*channel

	enabled atomic.Bool
}

// NewHub constructs a tracker hub. allowedOrigins restricts WebSocket upgrades
// (empty allows any origin); maxPeers caps the peer list returned on join.
func NewHub(log *slog.Logger, allowedOrigins []string, maxPeers int) *Hub {
	if log == nil {
		log = slog.Default()
	}
	if maxPeers <= 0 {
		maxPeers = 4
	}
	h := &Hub{
		log:      log,
		maxPeers: maxPeers,
		channels: map[string]*channel{},
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			CheckOrigin:     originChecker(allowedOrigins),
		},
	}
	h.enabled.Store(true)
	return h
}

// SetEnabled toggles whether new peers may join.
func (h *Hub) SetEnabled(v bool) { h.enabled.Store(v) }

// Enabled reports whether the tracker accepts peers.
func (h *Hub) Enabled() bool { return h.enabled.Load() }

// MaxPeers returns the configured peer-list cap.
func (h *Hub) MaxPeers() int { return h.maxPeers }

// PeerCount returns the total number of connected peers across channels.
func (h *Hub) PeerCount() int {
	h.mu.Lock()
	channels := make([]*channel, 0, len(h.channels))
	for _, ch := range h.channels {
		channels = append(channels, ch)
	}
	h.mu.Unlock()

	n := 0
	for _, ch := range channels {
		ch.mu.Lock()
		n += len(ch.peers)
		ch.mu.Unlock()
	}
	return n
}

type channel struct {
	id    string
	mu    sync.Mutex
	peers map[string]*peer
}

type peer struct {
	id      string
	channel *channel
	conn    *wsConn

	mu   sync.Mutex
	have map[string]struct{}
}

func (p *peer) addHave(segments []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range segments {
		if s != "" {
			p.have[s] = struct{}{}
		}
	}
}

func (p *peer) has(segment string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.have[segment]
	return ok
}

// HandleWS upgrades an HTTP request to a tracker WebSocket.
func (h *Hub) HandleWS(w http.ResponseWriter, r *http.Request) {
	if !h.enabled.Load() {
		http.Error(w, "p2p is disabled", http.StatusServiceUnavailable)
		return
	}
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.log.Warn("p2p upgrade failed", "err", err, "remote", r.RemoteAddr)
		return
	}
	c := &wsConn{conn: conn}
	h.serve(c)
}

type wsConn struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (c *wsConn) writeJSON(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteJSON(v)
}

func (c *wsConn) writeControl(messageType int, data []byte, deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteControl(messageType, data, deadline)
}

func (h *Hub) serve(c *wsConn) {
	defer c.conn.Close()

	var current *peer
	done := make(chan struct{})
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			close(done)
			if current != nil {
				h.removePeer(current)
			}
		})
	}
	defer cleanup()

	c.conn.SetReadLimit(1 << 20)
	_ = c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	})
	go func() {
		ticker := time.NewTicker(54 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := c.writeControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
					return
				}
			case <-done:
				return
			}
		}
	}()

	for {
		var msg Message
		if err := c.conn.ReadJSON(&msg); err != nil {
			return
		}
		if err := h.dispatch(c, &current, msg); err != nil {
			h.log.Debug("p2p message failed", "type", msg.Type, "err", err)
			_ = c.writeJSON(Message{Type: "error", Segment: msg.Segment})
		}
	}
}

func (h *Hub) dispatch(c *wsConn, current **peer, msg Message) error {
	switch msg.Type {
	case TypeJoin:
		if *current != nil {
			return nil
		}
		p, err := h.join(c, msg.Channel)
		if err != nil {
			return err
		}
		*current = p
		return nil
	case TypeLeave:
		if *current != nil {
			h.removePeer(*current)
			*current = nil
		}
		return nil
	}

	if *current == nil {
		return errors.New("p2p: not joined")
	}
	p := *current
	switch msg.Type {
	case TypeHave:
		p.addHave(msg.Segments)
		return nil
	case TypeWant:
		return h.handleWant(p, msg.Segment)
	case TypeSignal:
		return h.handleSignal(p, msg)
	default:
		return errors.New("p2p: unknown message type " + msg.Type)
	}
}

func (h *Hub) join(c *wsConn, channelID string) (*peer, error) {
	channelID = strings.TrimSpace(channelID)
	if channelID == "" {
		return nil, errors.New("p2p: join requires a channel")
	}

	h.mu.Lock()
	ch := h.channels[channelID]
	if ch == nil {
		ch = &channel{id: channelID, peers: map[string]*peer{}}
		h.channels[channelID] = ch
	}
	h.mu.Unlock()

	p := &peer{id: id.New("peer"), channel: ch, conn: c, have: map[string]struct{}{}}

	ch.mu.Lock()
	existing := make([]string, 0, len(ch.peers))
	for pid := range ch.peers {
		if len(existing) >= h.maxPeers {
			break
		}
		existing = append(existing, pid)
	}
	ch.peers[p.id] = p
	ch.mu.Unlock()

	// Tell existing peers a newcomer arrived (they wait for its offer).
	ch.broadcastExcept(p.id, Message{Type: TypePeerJoined, PeerID: p.id})

	if err := c.writeJSON(Message{Type: TypeJoined, PeerID: p.id, Channel: channelID, Peers: existing}); err != nil {
		h.removePeer(p)
		return nil, err
	}
	h.log.Info("p2p peer joined", "channel", channelID, "peer", p.id, "peers", len(existing))
	return p, nil
}

func (h *Hub) handleWant(p *peer, segment string) error {
	if segment == "" {
		return errors.New("p2p: want requires a segment")
	}
	ch := p.channel
	ch.mu.Lock()
	var peers []string
	for pid, other := range ch.peers {
		if pid == p.id {
			continue
		}
		if other.has(segment) {
			peers = append(peers, pid)
		}
	}
	ch.mu.Unlock()
	return p.conn.writeJSON(Message{Type: TypePeers, Segment: segment, Peers: peers})
}

func (h *Hub) handleSignal(p *peer, msg Message) error {
	if msg.To == "" {
		return errors.New("p2p: signal requires a target peer")
	}
	p.channel.mu.Lock()
	target := p.channel.peers[msg.To]
	p.channel.mu.Unlock()
	if target == nil {
		return errors.New("p2p: target peer not found")
	}
	msg.From = p.id
	msg.To = ""
	return target.conn.writeJSON(msg)
}

func (h *Hub) removePeer(p *peer) {
	ch := p.channel
	ch.mu.Lock()
	if _, ok := ch.peers[p.id]; !ok {
		ch.mu.Unlock()
		return
	}
	delete(ch.peers, p.id)
	empty := len(ch.peers) == 0
	ch.mu.Unlock()

	ch.broadcastExcept(p.id, Message{Type: TypePeerLeft, PeerID: p.id})

	if empty {
		h.mu.Lock()
		if len(ch.peers) == 0 {
			delete(h.channels, ch.id)
		}
		h.mu.Unlock()
	}
}

func (ch *channel) broadcastExcept(except string, msg Message) {
	ch.mu.Lock()
	targets := make([]*peer, 0, len(ch.peers))
	for pid, p := range ch.peers {
		if pid != except {
			targets = append(targets, p)
		}
	}
	ch.mu.Unlock()
	for _, p := range targets {
		_ = p.conn.writeJSON(msg)
	}
}

func originChecker(allowed []string) func(*http.Request) bool {
	if len(allowed) == 0 {
		return func(*http.Request) bool { return true }
	}
	set := make(map[string]struct{}, len(allowed))
	for _, o := range allowed {
		set[o] = struct{}{}
	}
	return func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		_, ok := set[origin]
		return ok
	}
}
