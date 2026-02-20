package tracing

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func newTestTracer() (trace.Tracer, *tracetest.InMemoryExporter) {
	mem := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(mem),
		sdktrace.WithResource(resource.NewSchemaless(
			attribute.String("service.name", "test"),
		)),
	)
	return tp.Tracer("test"), mem
}

func TestExportSpans_WritesValidJSONL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	exporter, err := NewJSONLExporter(path)
	if err != nil {
		t.Fatalf("NewJSONLExporter: %v", err)
	}
	defer exporter.Shutdown(context.Background())

	// Create real spans using the otel SDK.
	tracer, mem := newTestTracer()

	ctx, span := tracer.Start(context.Background(), "/hivepb.CoreService/SendMessage",
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.String("rpc.system", "grpc"),
			attribute.String("rpc.service", "hivepb.CoreService"),
			attribute.String("rpc.method", "SendMessage"),
			attribute.String("hive.pid", "3"),
		),
	)
	_ = ctx
	span.SetStatus(codes.Ok, "")
	span.End()

	// Get the recorded spans from the in-memory exporter.
	spans := mem.GetSpans()
	if len(spans) == 0 {
		t.Fatal("expected at least 1 span from in-memory exporter")
	}

	// Convert StubSpan to ReadOnlySpan via a snapshot approach:
	// Use a second TracerProvider with our JSONL exporter.
	tp2 := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	tracer2 := tp2.Tracer("test")
	_, span2 := tracer2.Start(context.Background(), "/hivepb.CoreService/SendMessage",
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.String("rpc.system", "grpc"),
			attribute.String("rpc.service", "hivepb.CoreService"),
			attribute.String("rpc.method", "SendMessage"),
			attribute.String("hive.pid", "3"),
		),
	)
	span2.SetStatus(codes.Ok, "")
	span2.End()

	// Force flush.
	tp2.Shutdown(context.Background())

	// Read and verify JSONL.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 1 {
		t.Fatalf("expected at least 1 line, got %d", len(lines))
	}

	var rec TraceRecord
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("unmarshal line: %v", err)
	}

	if rec.Operation != "/hivepb.CoreService/SendMessage" {
		t.Errorf("operation = %q, want /hivepb.CoreService/SendMessage", rec.Operation)
	}
	if rec.Kind != "server" {
		t.Errorf("kind = %q, want server", rec.Kind)
	}
	if rec.Status != "ok" {
		t.Errorf("status = %q, want ok", rec.Status)
	}
	if rec.TraceID == "" {
		t.Error("trace_id is empty")
	}
	if rec.SpanID == "" {
		t.Error("span_id is empty")
	}
	if rec.Attributes["rpc.system"] != "grpc" {
		t.Errorf("rpc.system = %q, want grpc", rec.Attributes["rpc.system"])
	}
	if rec.Attributes["hive.pid"] != "3" {
		t.Errorf("hive.pid = %q, want 3", rec.Attributes["hive.pid"])
	}
	if rec.DurationMS < 0 {
		t.Errorf("duration_ms = %d, want >= 0", rec.DurationMS)
	}

	// Verify timestamps are valid RFC3339.
	if _, err := time.Parse(time.RFC3339Nano, rec.StartTime); err != nil {
		t.Errorf("start_time %q not valid RFC3339Nano: %v", rec.StartTime, err)
	}
	if _, err := time.Parse(time.RFC3339Nano, rec.EndTime); err != nil {
		t.Errorf("end_time %q not valid RFC3339Nano: %v", rec.EndTime, err)
	}
}

func TestExportSpans_EmptySlice(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.jsonl")

	exporter, err := NewJSONLExporter(path)
	if err != nil {
		t.Fatalf("NewJSONLExporter: %v", err)
	}

	// Export empty slice should succeed without writing.
	if err := exporter.ExportSpans(context.Background(), nil); err != nil {
		t.Fatalf("ExportSpans(nil): %v", err)
	}

	exporter.Shutdown(context.Background())

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(data) != 0 {
		t.Errorf("expected empty file, got %d bytes", len(data))
	}
}

func TestShutdown_FlushesAndCloses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shutdown.jsonl")

	exporter, err := NewJSONLExporter(path)
	if err != nil {
		t.Fatalf("NewJSONLExporter: %v", err)
	}

	// Write a span via TracerProvider.
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	tracer := tp.Tracer("test")
	_, span := tracer.Start(context.Background(), "/test/Method")
	span.End()

	// Shutdown should flush everything.
	if err := tp.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// File should have content.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(data) == 0 {
		t.Error("expected non-empty file after shutdown")
	}

	// Subsequent export should not panic.
	err = exporter.ExportSpans(context.Background(), nil)
	if err != nil {
		t.Errorf("ExportSpans after shutdown: %v", err)
	}
}

func TestExportSpans_ErrorSpan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "error.jsonl")

	exporter, err := NewJSONLExporter(path)
	if err != nil {
		t.Fatalf("NewJSONLExporter: %v", err)
	}

	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	tracer := tp.Tracer("test")
	_, span := tracer.Start(context.Background(), "/hivepb.AgentService/Execute",
		trace.WithSpanKind(trace.SpanKindClient),
	)
	span.SetStatus(codes.Error, "connection refused")
	span.End()
	tp.Shutdown(context.Background())

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var rec TraceRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if rec.Status != "error" {
		t.Errorf("status = %q, want error", rec.Status)
	}
	if rec.StatusMessage != "connection refused" {
		t.Errorf("status_message = %q, want 'connection refused'", rec.StatusMessage)
	}
	if rec.Kind != "client" {
		t.Errorf("kind = %q, want client", rec.Kind)
	}
}
