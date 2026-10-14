package transcode

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// Manager owns the FFmpeg transcode sessions for the process.
type Manager struct {
	cfg Config
	log *slog.Logger

	mu       sync.Mutex
	sessions map[string]*Session
}

// New constructs a Manager with the given configuration.
func New(cfg Config, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{cfg: cfg.withDefaults(), log: log, sessions: map[string]*Session{}}
}

// Start launches an FFmpeg session for streamKey and returns its FLV sink. A
// session already running for the key is returned unchanged.
func (m *Manager) Start(streamKey string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if s, ok := m.sessions[streamKey]; ok {
		return s, nil
	}
	outDir := filepath.Join(m.cfg.OutputDir, streamKey)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("transcode: output dir: %w", err)
	}

	cmd := exec.Command(m.cfg.FFmpegPath, buildArgs(m.cfg, streamKey)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("transcode: stdin pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("transcode: start ffmpeg: %w", err)
	}

	s := &Session{
		streamKey: streamKey,
		cmd:       cmd,
		stdin:     stdin,
		flv:       newFLVWriter(stdin),
		log:       m.log.With("stream", streamKey),
		stderr:    &stderr,
	}
	m.sessions[streamKey] = s
	m.log.Info("transcode session started", "stream", streamKey, "renditions", len(m.cfg.Renditions))
	return s, nil
}

// Stop terminates the session for streamKey.
func (m *Manager) Stop(streamKey string) {
	m.mu.Lock()
	s, ok := m.sessions[streamKey]
	if ok {
		delete(m.sessions, streamKey)
	}
	m.mu.Unlock()
	if ok {
		s.Close()
	}
}

// StopAll terminates every session.
func (m *Manager) StopAll() {
	m.mu.Lock()
	sessions := m.sessions
	m.sessions = map[string]*Session{}
	m.mu.Unlock()
	for _, s := range sessions {
		s.Close()
	}
}

// Active returns the stream keys currently transcoding.
func (m *Manager) Active() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.sessions))
	for k := range m.sessions {
		out = append(out, k)
	}
	return out
}

// Session is one FFmpeg transcode process fed by an FLV byte stream.
type Session struct {
	streamKey string
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	flv       *flvWriter
	log       *slog.Logger
	stderr    *bytes.Buffer

	mu     sync.Mutex
	closed bool
}

// WriteFLVTag writes a raw FLV tag to the FFmpeg input. It satisfies
// rtmp.FLVSink so the RTMP session can forward media verbatim.
func (s *Session) WriteFLVTag(tagType uint8, timestamp uint32, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	return s.flv.writeTag(tagType, timestamp, data)
}

// Close closes the FFmpeg input and waits for the process to exit, killing it
// if it does not stop promptly.
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

	done := make(chan struct{})
	go func() {
		_ = s.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		<-done
	}

	if s.stderr != nil && s.stderr.Len() > 0 {
		s.log.Debug("ffmpeg exited", "stderr", s.stderr.String())
	}
	s.log.Info("transcode session stopped", "stream", s.streamKey)
}
