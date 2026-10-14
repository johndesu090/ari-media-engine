// Package logger provides a small structured logging constructor built on the
// standard library's log/slog package. Keeping the logger dependency-free makes
// it trivial to embed in every Ari Media Engine component.
package logger

import (
	"log/slog"
	"os"
	"strings"
)

// Options controls how a logger is constructed.
type Options struct {
	// Level is one of debug, info, warn, error. Anything else defaults to info.
	Level string
	// Format is either "json" or "text". Anything else defaults to json.
	Format string
}

// New builds a slog.Logger writing to stdout with the requested level/format.
func New(opts Options) *slog.Logger {
	handlerOpts := &slog.HandlerOptions{Level: parseLevel(opts.Level)}

	var handler slog.Handler
	if strings.EqualFold(opts.Format, "text") {
		handler = slog.NewTextHandler(os.Stdout, handlerOpts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, handlerOpts)
	}
	return slog.New(handler)
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
