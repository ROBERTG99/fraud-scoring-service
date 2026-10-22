package telemetry

import (
	"context"
	"encoding/hex"
	"log/slog"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// TracerName is the instrumentation scope of every span this module creates.
const TracerName = "github.com/ROBERTG99/fraud-scoring-service"

// propagator is W3C trace context only. Baggage is caller-controlled data nothing here reads, so it is
// neither accepted nor forwarded (security audit).
var propagator = propagation.TraceContext{}

// Sampler records every Score span, even under a caller's unsampled traceparent: the caller may join its
// trace but cannot hide its own requests from tracing (security audit).
func Sampler() sdktrace.Sampler {
	return sdktrace.ParentBased(sdktrace.AlwaysSample(), sdktrace.WithRemoteParentNotSampled(sdktrace.AlwaysSample()))
}

// InitTracing installs the global tracer provider and W3C propagator. Spans are exported over
// OTLP/gRPC only when OTEL_EXPORTER_OTLP_ENDPOINT (or the traces-specific variable) is set, for example to
// The `observability` profile's collector; otherwise tracing is a no-op and costs nothing on the hot path.
func InitTracing(ctx context.Context, service string) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagator)
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		// The noop provider directly, not the global delegate: no span, no lookup per call.
		otel.SetTracerProvider(noop.NewTracerProvider())
		return func(context.Context) error { return nil }, nil
	}
	exp, err := otlptracegrpc.New(ctx) // endpoint, insecure and headers come from the OTEL_EXPORTER_OTLP_* variables
	if err != nil {
		return nil, err
	}
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceName(service)))
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(
		// Batches leave the request path; a slow collector drops spans, never delays a decision.
		sdktrace.WithBatcher(exp, sdktrace.WithBatchTimeout(time.Second), sdktrace.WithMaxQueueSize(8192)),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(Sampler()),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// Propagator is the W3C trace-context propagator every service uses.
func Propagator() propagation.TextMapPropagator { return propagator }

// Tracer returns the module's tracer from the global provider.
func Tracer() trace.Tracer { return otel.Tracer(TracerName) }

// Traceparent returns the W3C traceparent of ctx's span, or "" when there is none. It formats the span
// context directly (one allocation) instead of injecting into a map carrier.
func Traceparent(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	var buf [55]byte // "00-" + 32 + "-" + 16 + "-" + 2
	tid, sid := sc.TraceID(), sc.SpanID()
	n := copy(buf[:], "00-")
	n += hex.Encode(buf[n:], tid[:])
	buf[n] = '-'
	n++
	n += hex.Encode(buf[n:], sid[:])
	buf[n] = '-'
	n++
	hex.Encode(buf[n:], []byte{byte(sc.TraceFlags() & trace.FlagsSampled)})
	return string(buf[:])
}

// ContextFromTraceparent returns ctx carrying the remote span context of a W3C traceparent.
func ContextFromTraceparent(ctx context.Context, traceparent string) context.Context {
	if traceparent == "" {
		return ctx
	}
	return propagator.Extract(ctx, propagation.MapCarrier{"traceparent": traceparent})
}

// SpanContextOf returns the span context in ctx (local or remote).
func SpanContextOf(ctx context.Context) trace.SpanContext { return trace.SpanContextFromContext(ctx) }

type traceHandler struct{ slog.Handler }

// TraceHandler adds trace_id and span_id to every record logged with a context that carries a span.
func TraceHandler(h slog.Handler) slog.Handler { return traceHandler{h} }

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{h.Handler.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{h.Handler.WithGroup(name)}
}
