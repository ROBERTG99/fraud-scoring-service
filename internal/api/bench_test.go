package api

import (
	"context"
	"fmt"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
)

// BenchmarkScore measures one Score with in-memory dependencies, tracing off (noop) and on (SDK, no
// exporter), so span overhead on the hot path shows in allocs/op.
func BenchmarkScore(b *testing.B) {
	for name, tracer := range map[string]trace.Tracer{
		"noop": noop.NewTracerProvider().Tracer("bench"),
		"sdk":  sdktrace.NewTracerProvider().Tracer("bench"),
	} {
		b.Run(name, func(b *testing.B) {
			h := newHarness(b)
			h.srv.d.Tracer = tracer
			ctx := context.Background()
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				i++
				tx := journeyTx(fmt.Sprintf("0192f000-0000-7000-8000-%012x", i))
				if _, err := h.srv.Score(ctx, &fraudv1.ScoreRequest{Transaction: tx}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
