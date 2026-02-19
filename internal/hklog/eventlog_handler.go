package hklog

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

// LogEmitter is the interface for emitting kernel internal logs as events.
// Implemented by King (avoids circular import with process package).
type LogEmitter interface {
	EmitLog(pid uint64, level, component, message string, fields map[string]string)
}

var (
	emitter   LogEmitter
	emitterMu sync.RWMutex
)

// SetEventLogEmitter registers a LogEmitter (typically King) so that
// kernel slog output at INFO+ is also emitted as ProcessEvent type="log".
func SetEventLogEmitter(e LogEmitter) {
	emitterMu.Lock()
	defer emitterMu.Unlock()
	emitter = e
}

// getEmitter returns the current emitter (or nil).
func getEmitter() LogEmitter {
	emitterMu.RLock()
	defer emitterMu.RUnlock()
	return emitter
}

// eventLogHandler is an slog.Handler that forwards INFO+ records to the
// EventLog via the registered LogEmitter.
type eventLogHandler struct {
	component string
	minLevel  slog.Level
}

// NewEventLogHandler creates a handler that emits to the EventLog.
// Only records at minLevel or above are emitted.
func NewEventLogHandler(minLevel slog.Level) slog.Handler {
	return &eventLogHandler{minLevel: minLevel}
}

func (h *eventLogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.minLevel
}

func (h *eventLogHandler) Handle(_ context.Context, r slog.Record) error {
	em := getEmitter()
	if em == nil {
		return nil
	}

	level := "debug"
	switch {
	case r.Level >= slog.LevelError:
		level = "error"
	case r.Level >= slog.LevelWarn:
		level = "warn"
	case r.Level >= slog.LevelInfo:
		level = "info"
	}

	// Extract component and other fields from attributes.
	component := h.component
	fields := make(map[string]string)
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "component" {
			component = a.Value.String()
		} else {
			fields[a.Key] = fmt.Sprint(a.Value.Any())
		}
		return true
	})

	em.EmitLog(0, level, component, r.Message, fields)
	return nil
}

func (h *eventLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	// Extract component from attrs if present.
	comp := h.component
	for _, a := range attrs {
		if a.Key == "component" {
			comp = a.Value.String()
		}
	}
	return &eventLogHandler{component: comp, minLevel: h.minLevel}
}

func (h *eventLogHandler) WithGroup(name string) slog.Handler {
	comp := h.component
	if comp == "" {
		comp = name
	} else {
		comp = comp + "." + name
	}
	return &eventLogHandler{component: comp, minLevel: h.minLevel}
}

var _ slog.Handler = (*eventLogHandler)(nil)

// AddEventLogHandler extends Init by appending the event log handler to the
// global slog default. Call this after Init() and after King is created.
func AddEventLogHandler() {
	mu.Lock()
	defer mu.Unlock()

	current := slog.Default().Handler()

	evtHandler := &eventLogHandler{minLevel: slog.LevelInfo}

	// If already a multiHandler, append to it.
	if mh, ok := current.(*multiHandler); ok {
		mh.handlers = append(mh.handlers, evtHandler)
		return
	}

	// Wrap current + event handler in a multiHandler.
	slog.SetDefault(slog.New(&multiHandler{
		handlers: []slog.Handler{current, evtHandler},
	}))
}

// levelFromString converts a level string to slog.Level (for use by callers).
func levelFromString(s string) slog.Level {
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
