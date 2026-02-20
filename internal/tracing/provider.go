package tracing

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// pidResolver is the package-level resolver set by SetPIDResolver.
var pidResolver PIDResolver

// Setup initializes the OpenTelemetry tracing pipeline:
// - Creates logs/otel/ directory
// - Opens a JSONL file for the session
// - Configures BatchSpanProcessor + TracerProvider
// - Sets the global TracerProvider
//
// Returns a shutdown function that flushes and closes the exporter.
func Setup(sessionTS, nodeName string) (shutdown func(context.Context), err error) {
	dir := filepath.Join("logs", "otel")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create otel log dir: %w", err)
	}

	path := filepath.Join(dir, sessionTS+".jsonl")
	exporter, err := NewJSONLExporter(path)
	if err != nil {
		return nil, err
	}

	bsp := sdktrace.NewBatchSpanProcessor(exporter,
		sdktrace.WithBatchTimeout(2*time.Second),
	)

	res := resource.NewSchemaless(
		attribute.String("service.name", "hivekernel"),
		attribute.String("hive.node", nodeName),
	)

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(bsp),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(tp)

	return func(ctx context.Context) {
		_ = tp.Shutdown(ctx)
	}, nil
}

// SetPIDResolver registers the resolver used by server interceptors
// to annotate spans with agent names. Call after King is created.
func SetPIDResolver(r PIDResolver) {
	pidResolver = r
}

// Tracer returns the package tracer for manual span creation.
func Tracer() trace.Tracer {
	return otel.Tracer(tracerName)
}
