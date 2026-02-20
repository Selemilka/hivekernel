package tracing

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// PIDResolver resolves a HiveKernel PID to an agent name.
// Avoids circular imports with the kernel package.
type PIDResolver interface {
	ResolveAgent(pid uint64) (name string, ok bool)
}

const tracerName = "hivekernel.grpc"

// parseMethod splits a gRPC full method like "/hivepb.CoreService/SendMessage"
// into service and method components.
func parseMethod(fullMethod string) (service, method string) {
	// fullMethod is "/package.Service/Method"
	fullMethod = strings.TrimPrefix(fullMethod, "/")
	if idx := strings.LastIndex(fullMethod, "/"); idx >= 0 {
		return fullMethod[:idx], fullMethod[idx+1:]
	}
	return fullMethod, ""
}

// rpcAttributes returns common RPC span attributes.
func rpcAttributes(fullMethod string) []attribute.KeyValue {
	service, method := parseMethod(fullMethod)
	return []attribute.KeyValue{
		attribute.String("rpc.system", "grpc"),
		attribute.String("rpc.service", service),
		attribute.String("rpc.method", method),
	}
}

// hiveAttributes reads hive-specific metadata from incoming gRPC context.
func hiveAttributes(ctx context.Context) []attribute.KeyValue {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil
	}
	var attrs []attribute.KeyValue
	if vals := md.Get("x-hivekernel-pid"); len(vals) > 0 {
		attrs = append(attrs, attribute.String("hive.pid", vals[0]))
	}
	return attrs
}

// resolveHiveAgent tries to resolve the agent name from the PID attribute.
func resolveHiveAgent(ctx context.Context, resolver PIDResolver) []attribute.KeyValue {
	if resolver == nil {
		return nil
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil
	}
	vals := md.Get("x-hivekernel-pid")
	if len(vals) == 0 {
		return nil
	}
	// Parse PID string to uint64.
	var pid uint64
	for _, c := range vals[0] {
		if c < '0' || c > '9' {
			return nil
		}
		pid = pid*10 + uint64(c-'0')
	}
	if name, ok := resolver.ResolveAgent(pid); ok {
		return []attribute.KeyValue{attribute.String("hive.agent", name)}
	}
	return nil
}

// recordError sets error status on a span if err is non-nil.
func recordError(span trace.Span, err error) {
	if err == nil {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	if st, ok := status.FromError(err); ok {
		span.SetAttributes(attribute.String("rpc.grpc.status_code", st.Code().String()))
	}
}

// ServerUnaryInterceptor returns a gRPC unary server interceptor that creates
// otel spans for incoming RPCs with HiveKernel attributes.
func ServerUnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		tracer := otel.Tracer(tracerName)
		ctx, span := tracer.Start(ctx, info.FullMethod, trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()

		span.SetAttributes(rpcAttributes(info.FullMethod)...)
		span.SetAttributes(hiveAttributes(ctx)...)
		span.SetAttributes(resolveHiveAgent(ctx, pidResolver)...)

		resp, err := handler(ctx, req)
		recordError(span, err)
		return resp, err
	}
}

// ServerStreamInterceptor returns a gRPC stream server interceptor that creates
// otel spans for incoming streaming RPCs.
func ServerStreamInterceptor() grpc.StreamServerInterceptor {
	return func(
		srv any,
		ss grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		tracer := otel.Tracer(tracerName)
		ctx, span := tracer.Start(ss.Context(), info.FullMethod, trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()

		span.SetAttributes(rpcAttributes(info.FullMethod)...)
		span.SetAttributes(hiveAttributes(ctx)...)
		span.SetAttributes(resolveHiveAgent(ctx, pidResolver)...)

		wrapped := &wrappedServerStream{ServerStream: ss, ctx: ctx}
		err := handler(srv, wrapped)
		recordError(span, err)
		return err
	}
}

// ClientUnaryInterceptor returns a gRPC unary client interceptor that creates
// otel spans for outgoing RPCs.
func ClientUnaryInterceptor() grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		method string,
		req, reply any,
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		tracer := otel.Tracer(tracerName)
		ctx, span := tracer.Start(ctx, method, trace.WithSpanKind(trace.SpanKindClient))
		defer span.End()

		span.SetAttributes(rpcAttributes(method)...)

		err := invoker(ctx, method, req, reply, cc, opts...)
		recordError(span, err)
		return err
	}
}

// ClientStreamInterceptor returns a gRPC stream client interceptor that creates
// otel spans for outgoing streaming RPCs.
func ClientStreamInterceptor() grpc.StreamClientInterceptor {
	return func(
		ctx context.Context,
		desc *grpc.StreamDesc,
		cc *grpc.ClientConn,
		method string,
		streamer grpc.Streamer,
		opts ...grpc.CallOption,
	) (grpc.ClientStream, error) {
		tracer := otel.Tracer(tracerName)
		ctx, span := tracer.Start(ctx, method, trace.WithSpanKind(trace.SpanKindClient))
		// Note: span ends when the stream RPC completes, but for simplicity
		// we end it after the initial connection. Stream-level tracing would
		// require wrapping the ClientStream, which is out of scope.
		defer span.End()

		cs, err := streamer(ctx, desc, cc, method, opts...)
		recordError(span, err)
		return cs, err
	}
}

// wrappedServerStream wraps a grpc.ServerStream to carry the span context.
type wrappedServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedServerStream) Context() context.Context {
	return w.ctx
}
