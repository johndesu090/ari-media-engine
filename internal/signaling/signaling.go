// Package signaling implements the WebRTC signaling transport. Clients speak a
// small JSON protocol over a WebSocket connection to publish or subscribe to a
// stream. ICE is gathered fully before the SDP is exchanged (non-trickle), which
// keeps the protocol simple and works through restrictive proxies.
package signaling

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/johndesu090/ari-media-engine/internal/broadcast"
	"github.com/johndesu090/ari-media-engine/internal/webrtc"
	"github.com/johndesu090/ari-media-engine/pkg/id"
)

// Message is the signaling envelope exchanged with clients.
type Message struct {
	Type          string  `json:"type"`
	StreamID      string  `json:"streamId,omitempty"`
	SessionID     string  `json:"sessionId,omitempty"`
	SDP           string  `json:"sdp,omitempty"`
	Candidate     string  `json:"candidate,omitempty"`
	SDPMid        *string `json:"sdpMid,omitempty"`
	SDPMLineIndex *uint16 `json:"sdpMLineIndex,omitempty"`
	Error         string  `json:"error,omitempty"`
}

// Message type constants.
const (
	TypePublish          = "publish"
	TypeAnswer           = "answer"
	TypeSubscribe        = "subscribe"
	TypeOffer            = "offer"
	TypeSubscriberAnswer = "subscriber-answer"
	TypeCandidate        = "candidate"
	TypeLeave            = "leave"
	TypePing             = "ping"
	TypePong             = "pong"
	TypeError            = "error"
)

// Hooks receive stream lifecycle notifications so the control plane can emit
// webhooks without the signaling package depending on it.
type Hooks struct {
	OnPublish func(streamID string)
	OnEnded   func(streamID string)
}

// Server upgrades HTTP requests to WebSocket signaling sessions.
type Server struct {
	engine *webrtc.Engine
	store  broadcast.Store
	log    *slog.Logger
	hooks  Hooks

	upgrader websocket.Upgrader

	writeWait  time.Duration
	pongWait   time.Duration
	pingPeriod time.Duration
	maxMessage int64
}

// Options configures a signaling Server.
type Options struct {
	Engine *webrtc.Engine
	Store  broadcast.Store
	Logger *slog.Logger
	Hooks  Hooks
	// AllowedOrigins restricts WebSocket upgrades. An empty list allows any
	// origin, which is appropriate when the HTTP layer already enforces the
	// embed policy.
	AllowedOrigins []string
}

// New constructs a signaling Server.
func New(opts Options) *Server {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		engine:     opts.Engine,
		store:      opts.Store,
		log:        log,
		hooks:      opts.Hooks,
		writeWait:  10 * time.Second,
		pongWait:   60 * time.Second,
		pingPeriod: 54 * time.Second,
		maxMessage: 1 << 20,
	}
	s.upgrader = websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin:     originChecker(opts.AllowedOrigins),
	}
	return s
}

// HandleWS is the http.HandlerFunc for the signaling endpoint.
func (s *Server) HandleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.log.Warn("signaling upgrade failed", "err", err, "remote", r.RemoteAddr)
		return
	}
	c := &wsConn{conn: conn}
	s.serve(c)
}

// wsConn serializes writes so the ping ticker and the read loop never interleave.
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

// session records what a connection registered so it can be torn down cleanly.
type session struct {
	streamID  string
	publisher bool
}

func (s *Server) serve(c *wsConn) {
	defer c.conn.Close()

	sessions := make(map[string]session)
	var mu sync.Mutex
	done := make(chan struct{})
	var once sync.Once

	closeAll := func() {
		once.Do(func() {
			close(done)
			mu.Lock()
			defer mu.Unlock()
			for sid, sess := range sessions {
				s.engine.CloseSession(sess.streamID, sid)
				if sess.publisher {
					s.markEnded(sess.streamID)
				}
			}
			sessions = make(map[string]session)
		})
	}
	defer closeAll()

	c.conn.SetReadLimit(s.maxMessage)
	_ = c.conn.SetReadDeadline(time.Now().Add(s.pongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(s.pongWait))
	})

	go func() {
		ticker := time.NewTicker(s.pingPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := c.writeControl(websocket.PingMessage, nil, time.Now().Add(s.writeWait)); err != nil {
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
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				s.log.Debug("signaling read ended", "err", err)
			}
			return
		}
		if err := s.dispatch(c, msg, sessions, &mu); err != nil {
			s.log.Warn("signaling message failed", "type", msg.Type, "stream", msg.StreamID, "err", err)
			_ = c.writeJSON(Message{Type: TypeError, StreamID: msg.StreamID, SessionID: msg.SessionID, Error: err.Error()})
		}
	}
}

func (s *Server) dispatch(c *wsConn, msg Message, sessions map[string]session, mu *sync.Mutex) error {
	switch msg.Type {
	case TypePublish:
		return s.handlePublish(c, msg, sessions, mu)
	case TypeSubscribe:
		return s.handleSubscribe(c, msg, sessions, mu)
	case TypeSubscriberAnswer:
		return s.engine.SetSubscriberAnswer(msg.StreamID, msg.SessionID, msg.SDP)
	case TypeCandidate:
		return s.engine.AddICECandidate(msg.StreamID, msg.SessionID, webrtc.ICECandidate{
			Candidate:     msg.Candidate,
			SDPMid:        msg.SDPMid,
			SDPMLineIndex: msg.SDPMLineIndex,
		})
	case TypeLeave:
		return s.handleLeave(msg, sessions, mu)
	case TypePing:
		return c.writeJSON(Message{Type: TypePong})
	case "":
		return errors.New("signaling: missing message type")
	default:
		return errors.New("signaling: unknown message type " + msg.Type)
	}
}

func (s *Server) handlePublish(c *wsConn, msg Message, sessions map[string]session, mu *sync.Mutex) error {
	if msg.StreamID == "" || msg.SDP == "" {
		return errors.New("signaling: publish requires streamId and sdp")
	}
	sessionID := msg.SessionID
	if sessionID == "" {
		sessionID = newSessionID()
	}
	answer, err := s.engine.Publish(msg.StreamID, sessionID, msg.SDP, s.candidateRelay(c, msg.StreamID, sessionID))
	if err != nil {
		return err
	}
	mu.Lock()
	sessions[sessionID] = session{streamID: msg.StreamID, publisher: true}
	mu.Unlock()

	s.markLive(msg.StreamID)
	return c.writeJSON(Message{Type: TypeAnswer, StreamID: msg.StreamID, SessionID: sessionID, SDP: answer})
}

func (s *Server) handleSubscribe(c *wsConn, msg Message, sessions map[string]session, mu *sync.Mutex) error {
	if msg.StreamID == "" {
		return errors.New("signaling: subscribe requires streamId")
	}
	if !s.webRTCEnabled(msg.StreamID) {
		return errors.New("signaling: stream is not available over WebRTC")
	}
	sessionID := msg.SessionID
	if sessionID == "" {
		sessionID = newSessionID()
	}
	offer, err := s.engine.Subscribe(msg.StreamID, sessionID, s.candidateRelay(c, msg.StreamID, sessionID))
	if err != nil {
		return err
	}
	mu.Lock()
	sessions[sessionID] = session{streamID: msg.StreamID}
	mu.Unlock()

	return c.writeJSON(Message{Type: TypeOffer, StreamID: msg.StreamID, SessionID: sessionID, SDP: offer})
}

func (s *Server) handleLeave(msg Message, sessions map[string]session, mu *sync.Mutex) error {
	mu.Lock()
	sess, ok := sessions[msg.SessionID]
	if ok {
		delete(sessions, msg.SessionID)
	}
	mu.Unlock()
	if !ok {
		return nil
	}
	s.engine.CloseSession(sess.streamID, msg.SessionID)
	if sess.publisher {
		s.markEnded(sess.streamID)
	}
	return nil
}

func (s *Server) markLive(streamID string) {
	s.setStatus(streamID, broadcast.StatusLive)
	if s.hooks.OnPublish != nil {
		s.hooks.OnPublish(streamID)
	}
}

func (s *Server) markEnded(streamID string) {
	s.setStatus(streamID, broadcast.StatusEnded)
	if s.hooks.OnEnded != nil {
		s.hooks.OnEnded(streamID)
	}
}

func (s *Server) setStatus(streamID string, status broadcast.Status) {
	if s.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	b, err := s.store.GetByStreamID(ctx, streamID)
	if err != nil {
		return
	}
	b.SetStatus(status)
}

// webRTCEnabled reports whether WebRTC playback is allowed. WebRTC playback has
// been removed; streams are delivered over HLS/DASH only.
func (s *Server) webRTCEnabled(streamID string) bool {
	return false
}

// candidateRelay returns a callback that forwards trickled ICE candidates to
// the client over the signaling connection.
func (s *Server) candidateRelay(c *wsConn, streamID, sessionID string) func(webrtc.ICECandidate) {
	return func(cand webrtc.ICECandidate) {
		_ = c.writeJSON(Message{
			Type:          TypeCandidate,
			StreamID:      streamID,
			SessionID:     sessionID,
			Candidate:     cand.Candidate,
			SDPMid:        cand.SDPMid,
			SDPMLineIndex: cand.SDPMLineIndex,
		})
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

func newSessionID() string {
	return id.New("sess")
}
