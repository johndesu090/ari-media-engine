// Package audio transcodes a publisher's FLV audio (AAC) to Opus and feeds the
// result into the WebRTC engine as RTP, so WebRTC subscribers receive audio.
// Browsers do not negotiate AAC, so a transcode step is required.
package audio

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os/exec"
	"sync"
	"time"

	"github.com/pion/rtp"
	pion "github.com/pion/webrtc/v4"

	"github.com/johndesu090/ari-media-engine/internal/flv"
)

// Engine is the subset of the WebRTC ingest session the audio pipeline needs.
type Engine interface {
	EnsureAudioTrack() (*pion.TrackLocalStaticRTP, error)
	WriteAudioRTP(pkt *rtp.Packet) error
}

// Manager spawns FFmpeg audio transcode sessions.
type Manager struct {
	ffmpeg  string
	bitrate string
	log     *slog.Logger

	mu       sync.Mutex
	sessions map[string]*Session
}

// Options configures a Manager.
type Options struct {
	FFmpegPath string
	Bitrate    string
	Logger     *slog.Logger
}

// NewManager constructs an audio Manager.
func NewManager(opts Options) *Manager {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	ffmpeg := opts.FFmpegPath
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	bitrate := opts.Bitrate
	if bitrate == "" {
		bitrate = "96k"
	}
	return &Manager{ffmpeg: ffmpeg, bitrate: bitrate, log: log, sessions: map[string]*Session{}}
}

// StopAll stops every audio session (idempotent).
func (m *Manager) StopAll() {
	m.mu.Lock()
	sessions := m.sessions
	m.sessions = map[string]*Session{}
	m.mu.Unlock()
	for _, s := range sessions {
		s.Close()
	}
}

// Active returns the stream keys with a running audio transcode.
func (m *Manager) Active() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.sessions))
	for k := range m.sessions {
		out = append(out, k)
	}
	return out
}

// Start launches an audio transcode session for streamKey that feeds engine.
// The raw FLV stream (already produced by the RTMP demuxer) is piped to FFmpeg,
// which outputs Opus RTP to a loopback UDP port the session reads back.
func (m *Manager) Start(streamKey string, engine Engine) (*Session, error) {
	if _, err := engine.EnsureAudioTrack(); err != nil {
		return nil, err
	}

	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		return nil, fmt.Errorf("audio: rtp socket: %w", err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port

	args := []string{
		"-hide_banner", "-loglevel", "warning",
		"-probesize", "500000", "-analyzeduration", "1000000",
		"-f", "flv", "-i", "pipe:0",
		"-vn",
		"-c:a", "libopus",
		"-b:a", m.bitrate,
		"-ar", "48000", "-ac", "2",
		"-application", "lowdelay",
		"-f", "rtp",
		"-payload_type", "111",
		fmt.Sprintf("rtp://127.0.0.1:%d?pkt_size=1200", port),
	}
	cmd := exec.Command(m.ffmpeg, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("audio: stdin pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("audio: start ffmpeg: %w", err)
	}

	s := &Session{
		streamKey: streamKey,
		cmd:       cmd,
		stdin:     stdin,
		flv:       flv.NewWriter(stdin),
		conn:      conn,
		engine:    engine,
		log:       m.log.With("stream", streamKey),
		stderr:    &stderr,
	}
	go s.readLoop()

	m.mu.Lock()
	m.sessions[streamKey] = s
	m.mu.Unlock()

	m.log.Info("audio transcode started", "stream", streamKey, "rtpPort", port)
	return s, nil
}

// Session is one audio transcode job. It satisfies rtmp.FLVSink.
type Session struct {
	streamKey string
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	flv       *flv.Writer
	conn      *net.UDPConn
	engine    Engine
	log       *slog.Logger
	stderr    *bytes.Buffer

	mu     sync.Mutex
	closed bool
	once   sync.Once
}

// WriteFLVTag pipes a raw FLV tag to the audio transcoder.
func (s *Session) WriteFLVTag(tagType uint8, timestamp uint32, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	return s.flv.WriteTag(tagType, timestamp, data)
}

func (s *Session) readLoop() {
	buf := make([]byte, 2048)
	for {
		_ = s.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		n, _, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}
		s.once.Do(func() { s.log.Info("audio RTP flowing", "stream", s.streamKey) })
		if err := s.engine.WriteAudioRTP(pkt); err != nil {
			return
		}
	}
}

// Close stops the transcoder and its RTP reader.
func (s *Session) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	stdin := s.stdin
	s.mu.Unlock()

	if stdin != nil {
		_ = stdin.Close()
	}
	_ = s.conn.Close()

	done := make(chan struct{})
	go func() {
		_ = s.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		<-done
	}
	if s.stderr != nil && s.stderr.Len() > 0 {
		s.log.Debug("audio ffmpeg exited", "stderr", s.stderr.String())
	}
	s.log.Info("audio transcode stopped", "stream", s.streamKey)
}
