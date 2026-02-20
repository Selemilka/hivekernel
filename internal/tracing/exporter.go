package tracing

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// TraceRecord is the JSONL schema written to logs/otel/*.jsonl.
type TraceRecord struct {
	TraceID       string            `json:"trace_id"`
	SpanID        string            `json:"span_id"`
	ParentSpanID  string            `json:"parent_span_id,omitempty"`
	Operation     string            `json:"operation"`
	Kind          string            `json:"kind"`
	StartTime     string            `json:"start_time"`
	EndTime       string            `json:"end_time"`
	DurationMS    int64             `json:"duration_ms"`
	Status        string            `json:"status"`
	StatusMessage string            `json:"status_message,omitempty"`
	Attributes    map[string]string `json:"attributes,omitempty"`
}

// JSONLExporter implements sdktrace.SpanExporter, writing spans as JSONL.
type JSONLExporter struct {
	mu     sync.Mutex
	file   *os.File
	writer *bufio.Writer
}

// NewJSONLExporter creates an exporter that appends trace records to the given file path.
func NewJSONLExporter(path string) (*JSONLExporter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("open trace log: %w", err)
	}
	return &JSONLExporter{
		file:   f,
		writer: bufio.NewWriter(f),
	}, nil
}

// ExportSpans converts each span to a TraceRecord and writes it as a JSON line.
func (e *JSONLExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	if len(spans) == 0 {
		return nil
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.writer == nil {
		return nil
	}

	for _, span := range spans {
		rec := spanToRecord(span)
		data, err := json.Marshal(rec)
		if err != nil {
			continue
		}
		e.writer.Write(data)
		e.writer.WriteByte('\n')
	}
	return e.writer.Flush()
}

// Shutdown flushes pending data and closes the file.
func (e *JSONLExporter) Shutdown(_ context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.writer != nil {
		e.writer.Flush()
	}
	if e.file != nil {
		err := e.file.Close()
		e.file = nil
		e.writer = nil
		return err
	}
	return nil
}

// spanToRecord converts a ReadOnlySpan to our JSONL record format.
func spanToRecord(s sdktrace.ReadOnlySpan) TraceRecord {
	sc := s.SpanContext()
	parent := s.Parent()

	var parentID string
	if parent.HasSpanID() {
		parentID = parent.SpanID().String()
	}

	// Map otel status code to lowercase string.
	// codes.Unset=0, codes.Error=1, codes.Ok=2
	var status string
	switch s.Status().Code {
	case 0: // Unset
		status = "unset"
	case 1: // Error
		status = "error"
	case 2: // Ok
		status = "ok"
	default:
		status = "unset"
	}

	attrs := make(map[string]string, len(s.Attributes()))
	for _, kv := range s.Attributes() {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}

	start := s.StartTime()
	end := s.EndTime()

	return TraceRecord{
		TraceID:       sc.TraceID().String(),
		SpanID:        sc.SpanID().String(),
		ParentSpanID:  parentID,
		Operation:     s.Name(),
		Kind:          s.SpanKind().String(),
		StartTime:     start.Format(time.RFC3339Nano),
		EndTime:       end.Format(time.RFC3339Nano),
		DurationMS:    end.Sub(start).Milliseconds(),
		Status:        status,
		StatusMessage: s.Status().Description,
		Attributes:    attrs,
	}
}
