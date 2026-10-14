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

	"github.com/johndesu090/ari-media-engine/internal/flv"
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

// Start launches the FFmpeg processes for streamKey producing the requested
// formats, and returns their shared FLV sink. A session already running for the
// key is returned unchanged. A zero format set returns (nil, nil).
func (m *Manager) Start(streamKey string, formats Format) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if s, ok := m.sessions[streamKey]; ok {
		return s, nil
	}
	if formats == 0 {
		return nil, nil
	}

	outDir := filepath.Join(m.cfg.OutputDir, streamKey)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("transcode: output dir: %w", err)
	}
	if formats&FormatHLS != 0 {
		if err := writeHLSMaster(outDir, m.cfg.Renditions); err != nil {
			return nil, fmt.Errorf("transcode: master playlist: %w", err)
		}
	}

	var argSets [][]string
	if formats&FormatHLS != 0 {
		argSets = append(argSets, buildHLSArgs(m.cfg, streamKey))
	}
	if formats&FormatDASH != 0 {
		argSets = append(argSets, buildDASHArgs(m.cfg, streamKey))
	}

	s := &Session{streamKey: streamKey, log: m.log.With("stream", streamKey)}
	var writers []io.Writer
	for _, args := range argSets {
		cmd := exec.Command(m.cfg.FFmpegPath, args...)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			s.closeProcs()
			return nil, fmt.Errorf("transcode: stdin pipe: %w", err)
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			_ = stdin.Close()
			s.closeProcs()
			return nil, fmt.Errorf("transcode: start ffmpeg: %w", err)
		}
		s.procs = append(s.procs, &proc{cmd: cmd, stdin: stdin, stderr: &stderr})
		writers = append(writers, stdin)
	}

	s.flv = flv.NewWriter(io.MultiWriter(writers...))
	m.sessions[streamKey] = s
	m.log.Info("transcode session started", "stream", streamKey, "dir", outDir, "renditions", len(m.cfg.Renditions), "formats", formatNames(formats))
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

func formatNames(f Format) string {
	var names []string
	if f&FormatHLS != 0 {
		names = append(names, "hls")
	}
	if f&FormatDASH != 0 {
		names = append(names, "dash")
	}
	return fmt.Sprint(names)
}

// proc is one FFmpeg process with its stdin and captured stderr.
type proc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr *bytes.Buffer
}

// Session is one logical transcode job (one or more FFmpeg processes) fed by a
// shared FLV byte stream.
type Session struct {
	streamKey string
	procs     []*proc
	flv       *flv.Writer
	log       *slog.Logger

	mu     sync.Mutex
	closed bool
}

// WriteFLVTag writes a raw FLV tag to every FFmpeg input. It satisfies
// rtmp.FLVSink so the RTMP session can forward media verbatim.
func (s *Session) WriteFLVTag(tagType uint8, timestamp uint32, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.flv == nil {
		return nil
	}
	return s.flv.WriteTag(tagType, timestamp, data)
}

// Close closes every FFmpeg input and waits for the processes to exit, killing
// any that do not stop promptly.
func (s *Session) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	procs := s.procs
	s.mu.Unlock()

	for _, p := range procs {
		if p.stdin != nil {
			_ = p.stdin.Close()
		}
	}

	done := make(chan struct{})
	go func() {
		for _, p := range procs {
			_ = p.cmd.Wait()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		for _, p := range procs {
			if p.cmd.Process != nil {
				_ = p.cmd.Process.Kill()
			}
		}
		<-done
	}

	for _, p := range procs {
		if p.stderr != nil && p.stderr.Len() > 0 {
			s.log.Warn("ffmpeg exited", "stream", s.streamKey, "stderr", p.stderr.String())
		}
	}
	s.log.Info("transcode session stopped", "stream", s.streamKey)
}

func (s *Session) closeProcs() {
	for _, p := range s.procs {
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
	}
}
