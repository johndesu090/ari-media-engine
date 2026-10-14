// Package recorder captures a published stream's raw FLV to disk and uploads
// the finished recording to the configured storage backend (local or S3).
package recorder

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/johndesu090/ari-media-engine/internal/flv"
	"github.com/johndesu090/ari-media-engine/internal/storage"
)

// Manager creates and tracks recording sessions.
type Manager struct {
	store   storage.Store
	workDir string
	prefix  string
	log     *slog.Logger

	mu       sync.Mutex
	sessions map[string]*Session
}

// Options configures a Manager.
type Options struct {
	Store   storage.Store
	WorkDir string
	Prefix  string
	Logger  *slog.Logger
}

// New constructs a recording Manager.
func New(opts Options) *Manager {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	workDir := opts.WorkDir
	if workDir == "" {
		workDir = filepath.Join(os.TempDir(), "ari-recordings")
	}
	return &Manager{store: opts.Store, workDir: workDir, prefix: strings.Trim(opts.Prefix, "/"), log: log, sessions: map[string]*Session{}}
}

// Start begins recording streamKey and returns its FLV sink. A session already
// running for the key is returned unchanged.
func (m *Manager) Start(streamKey string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if s, ok := m.sessions[streamKey]; ok {
		return s, nil
	}
	if err := os.MkdirAll(m.workDir, 0o755); err != nil {
		return nil, fmt.Errorf("recorder: work dir: %w", err)
	}

	name := fmt.Sprintf("%s-%s.flv", sanitize(streamKey), time.Now().UTC().Format("20060102T150405Z"))
	local := filepath.Join(m.workDir, name)
	f, err := os.Create(local)
	if err != nil {
		return nil, fmt.Errorf("recorder: create file: %w", err)
	}

	s := &Session{
		streamKey: streamKey,
		key:       filepath.ToSlash(filepath.Join(m.prefix, sanitize(streamKey), name)),
		localPath: local,
		file:      f,
		flv:       flv.NewWriter(f),
		store:     m.store,
		log:       m.log.With("stream", streamKey),
	}
	m.sessions[streamKey] = s
	m.log.Info("recording started", "stream", streamKey, "key", s.key)
	return s, nil
}

// Stop finalizes and uploads the recording for streamKey.
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

// StopAll finalizes every session.
func (m *Manager) StopAll() {
	m.mu.Lock()
	sessions := m.sessions
	m.sessions = map[string]*Session{}
	m.mu.Unlock()
	for _, s := range sessions {
		s.Close()
	}
}

// Active returns the stream keys currently recording.
func (m *Manager) Active() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.sessions))
	for k := range m.sessions {
		out = append(out, k)
	}
	return out
}

// Session is one recording, fed by the stream's raw FLV tags. It satisfies
// rtmp.FLVSink.
type Session struct {
	streamKey string
	key       string
	localPath string
	file      *os.File
	flv       *flv.Writer
	store     storage.Store
	log       *slog.Logger

	mu     sync.Mutex
	closed bool
}

// Key returns the storage key the recording will be uploaded to.
func (s *Session) Key() string { return s.key }

// WriteFLVTag appends a raw FLV tag to the recording.
func (s *Session) WriteFLVTag(tagType uint8, timestamp uint32, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	return s.flv.WriteTag(tagType, timestamp, data)
}

// Close finalizes the file and uploads it to storage.
func (s *Session) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	file := s.file
	s.mu.Unlock()

	if file != nil {
		_ = file.Sync()
		_ = file.Close()
	}

	if s.store != nil {
		s.upload()
	}
	_ = os.Remove(s.localPath)
	s.log.Info("recording stopped", "stream", s.streamKey)
}

func (s *Session) upload() {
	f, err := os.Open(s.localPath)
	if err != nil {
		s.log.Error("recording open failed", "err", err, "path", s.localPath)
		return
	}
	defer f.Close()

	var size int64
	if info, err := f.Stat(); err == nil {
		size = info.Size()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := s.store.Put(ctx, s.key, f, size, "video/x-flv"); err != nil {
		s.log.Error("recording upload failed", "err", err, "key", s.key)
		return
	}
	s.log.Info("recording uploaded", "key", s.key, "bytes", size, "url", s.store.URL(s.key))
}

// sanitize keeps only filesystem-safe characters in a stream key.
func sanitize(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "stream"
	}
	return string(out)
}
