// Package hklog provides structured logging for HiveKernel using log/slog.
package hklog

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
)

var (
	levelVar slog.LevelVar
	mu       sync.Mutex
	inited   bool
)

// Init configures the global slog logger.
// level: "debug", "info", "warn", "error" (default: "info").
// logFile: optional path to a JSON log file (empty = no file output).
func Init(level string, logFile string) {
	mu.Lock()
	defer mu.Unlock()

	levelVar.Set(parseLevel(level))

	var hs []slog.Handler

	// Console handler: text, stderr.
	consoleOpts := &slog.HandlerOptions{Level: &levelVar}
	hs = append(hs, slog.NewTextHandler(os.Stderr, consoleOpts))

	// Optional JSON file handler.
	if logFile != "" {
		f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err == nil {
			fileOpts := &slog.HandlerOptions{Level: &levelVar}
			hs = append(hs, slog.NewJSONHandler(f, fileOpts))
		}
	}

	if len(hs) == 1 {
		slog.SetDefault(slog.New(hs[0]))
	} else {
		slog.SetDefault(slog.New(&multiHandler{handlers: hs}))
	}
	inited = true
}

// For returns a logger tagged with the given component name.
func For(component string) *slog.Logger {
	return slog.Default().With("component", component)
}

// SetLevel changes the log level dynamically.
func SetLevel(level string) {
	levelVar.Set(parseLevel(level))
}

// Level returns the current log level string.
func Level() string {
	l := levelVar.Level()
	switch {
	case l <= slog.LevelDebug:
		return "debug"
	case l <= slog.LevelInfo:
		return "info"
	case l <= slog.LevelWarn:
		return "warn"
	default:
		return "error"
	}
}

// ParseLevel converts a level string to slog.Level. Exported for testing.
func ParseLevel(s string) slog.Level {
	return parseLevel(s)
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
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

// multiHandler fans out log records to multiple handlers.
type multiHandler struct {
	handlers []slog.Handler
}

func (m *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var firstErr error
	for _, h := range m.handlers {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	hs := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		hs[i] = h.WithAttrs(attrs)
	}
	return &multiHandler{handlers: hs}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	hs := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		hs[i] = h.WithGroup(name)
	}
	return &multiHandler{handlers: hs}
}

var _ slog.Handler = (*multiHandler)(nil)
