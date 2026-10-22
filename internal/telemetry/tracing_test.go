package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestLogsCarryTheTraceID(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(TraceHandler(slog.NewJSONHandler(&buf, nil)))
	tp := sdktrace.NewTracerProvider()
	ctx, span := tp.Tracer("t").Start(context.Background(), "op")
	log.InfoContext(ctx, "hello")
	span.End()
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatal(err)
	}
	if line["trace_id"] != span.SpanContext().TraceID().String() || line["span_id"] != span.SpanContext().SpanID().String() {
		t.Fatalf("log line %v", line)
	}
	buf.Reset()
	log.Info("no context")
	if bytes.Contains(buf.Bytes(), []byte("trace_id")) {
		t.Fatalf("trace_id without a span: %s", buf.String())
	}
}

func TestTracingIsANoOpWithoutAnEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	shutdown, err := InitTracing(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	_, span := Tracer().Start(context.Background(), "op")
	defer span.End()
	if span.SpanContext().IsSampled() {
		t.Fatal("spans are recorded without an exporter")
	}
}

func TestTraceparentRoundTrip(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	ctx, span := tp.Tracer("t").Start(context.Background(), "op")
	defer span.End()
	tp2 := Traceparent(ctx)
	got := ContextFromTraceparent(context.Background(), tp2)
	if tp2 == "" || SpanContextOf(got).TraceID() != span.SpanContext().TraceID() {
		t.Fatalf("traceparent %q did not round-trip", tp2)
	}
	if Traceparent(context.Background()) != "" {
		t.Fatal("a context without a span has no traceparent")
	}
}

// A caller cannot switch off the scorer's spans by sending an unsampled traceparent (security audit).
func TestRemoteUnsampledParentIsStillSampled(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(Sampler()), sdktrace.WithSpanProcessor(rec))
	ctx := ContextFromTraceparent(context.Background(), "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00")
	_, span := tp.Tracer("t").Start(ctx, "fraud.score")
	span.End()
	ended := rec.Ended()
	if len(ended) != 1 || !ended[0].SpanContext().IsSampled() {
		t.Fatalf("spans %d, sampled %v", len(ended), len(ended) == 1 && ended[0].SpanContext().IsSampled())
	}
	if ended[0].Parent().SpanID().String() != "00f067aa0ba902b7" {
		t.Fatal("the span must still join the caller's trace")
	}
}

// Baggage is caller-controlled data with no use here: it is not propagated.
func TestPropagatorCarriesNoBaggage(t *testing.T) {
	for _, f := range Propagator().Fields() {
		if f == "baggage" {
			t.Fatal("baggage is propagated")
		}
	}
}

// Traceparent formats the span context directly; it matches the W3C propagator's encoding.
func TestTraceparentMatchesThePropagator(t *testing.T) {
	for _, sampled := range []bool{true, false} {
		sampler := sdktrace.NeverSample()
		if sampled {
			sampler = sdktrace.AlwaysSample()
		}
		ctx, span := sdktrace.NewTracerProvider(sdktrace.WithSampler(sampler)).Tracer("t").Start(context.Background(), "op")
		carrier := propagation.MapCarrier{}
		propagation.TraceContext{}.Inject(ctx, carrier)
		if got := Traceparent(ctx); got != carrier.Get("traceparent") {
			t.Fatalf("sampled=%v: %q, propagator %q", sampled, got, carrier.Get("traceparent"))
		}
		span.End()
	}
}

func BenchmarkTraceparent(b *testing.B) {
	ctx, span := sdktrace.NewTracerProvider().Tracer("t").Start(context.Background(), "op")
	defer span.End()
	b.ReportAllocs()
	for b.Loop() {
		_ = Traceparent(ctx)
	}
}
