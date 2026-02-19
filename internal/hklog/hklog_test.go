package hklog

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	tests := []struct {
		input string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"INFO", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"ERROR", slog.LevelError},
		{"", slog.LevelInfo},
		{"unknown", slog.LevelInfo},
	}
	for _, tt := range tests {
		got := ParseLevel(tt.input)
		if got != tt.want {
			t.Errorf("ParseLevel(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestFor(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	slog.SetDefault(slog.New(h))

	logger := For("test-component")
	logger.Info("hello", "key", "value")

	output := buf.String()
	if !strings.Contains(output, "component=test-component") {
		t.Errorf("expected component attr, got: %s", output)
	}
	if !strings.Contains(output, "hello") {
		t.Errorf("expected message, got: %s", output)
	}
	if !strings.Contains(output, "key=value") {
		t.Errorf("expected key=value, got: %s", output)
	}
}

func TestSetLevel(t *testing.T) {
	// Start at info.
	levelVar.Set(slog.LevelInfo)
	if Level() != "info" {
		t.Errorf("expected info, got %s", Level())
	}

	SetLevel("debug")
	if Level() != "debug" {
		t.Errorf("expected debug, got %s", Level())
	}

	SetLevel("error")
	if Level() != "error" {
		t.Errorf("expected error, got %s", Level())
	}

	SetLevel("warn")
	if Level() != "warn" {
		t.Errorf("expected warn, got %s", Level())
	}
}

func TestMultiHandler(t *testing.T) {
	var buf1, buf2 bytes.Buffer
	h1 := slog.NewTextHandler(&buf1, &slog.HandlerOptions{Level: slog.LevelDebug})
	h2 := slog.NewJSONHandler(&buf2, &slog.HandlerOptions{Level: slog.LevelDebug})

	mh := &multiHandler{handlers: []slog.Handler{h1, h2}}

	logger := slog.New(mh)
	logger.Info("multi-test", "k", "v")

	// Both buffers should have output.
	if !strings.Contains(buf1.String(), "multi-test") {
		t.Errorf("text handler missing output: %s", buf1.String())
	}
	if !strings.Contains(buf2.String(), "multi-test") {
		t.Errorf("json handler missing output: %s", buf2.String())
	}
}

func TestMultiHandlerEnabled(t *testing.T) {
	h1 := slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelError})
	h2 := slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelDebug})

	mh := &multiHandler{handlers: []slog.Handler{h1, h2}}

	// Should be enabled because h2 accepts debug.
	if !mh.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("expected Enabled=true for debug (h2 accepts it)")
	}
}

func TestMultiHandlerWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	mh := &multiHandler{handlers: []slog.Handler{h}}

	mh2 := mh.WithAttrs([]slog.Attr{slog.String("extra", "val")})
	logger := slog.New(mh2)
	logger.Info("attr-test")

	if !strings.Contains(buf.String(), "extra=val") {
		t.Errorf("expected extra=val, got: %s", buf.String())
	}
}
