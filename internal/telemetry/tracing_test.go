package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
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
