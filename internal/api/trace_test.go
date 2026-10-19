package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/store"
)

func traced(t *testing.T) (*harness, *tracetest.SpanRecorder, trace.Tracer) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	h := newHarness(t)
	h.srv.d.Tracer = tp.Tracer("test")
	return h, rec, tp.Tracer("test")
}

func attrs(s sdktrace.ReadOnlySpan) map[attribute.Key]attribute.Value {
	m := map[attribute.Key]attribute.Value{}
	for _, kv := range s.Attributes() {
		m[kv.Key] = kv.Value
	}
	return m
}

func TestScoreSpanHasDecisionAttributes(t *testing.T) {
	h, rec, tracer := traced(t)
	ctx, parent := tracer.Start(context.Background(), "fraud.v1.Scorer/Score")
	d, err := h.srv.Score(ctx, &fraudv1.ScoreRequest{Transaction: journeyTx("0192f000-0000-7000-8000-0000000000e1")})
	parent.End()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range rec.Ended() {
		byName[s.Name()] = s
	}
	score, ok := byName["fraud.score"]
	if !ok {
		t.Fatalf("no fraud.score span among %v", byName)
	}
	if score.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Fatal("fraud.score is not a child of the gRPC server span")
	}
	a := attrs(score)
	if a["app.decision.action"].AsString() != "REVIEW" || a["app.decision.id"].AsString() != d.DecisionId ||
		a["app.decision.degraded"].AsBool() || a["app.rules_version"].AsString() != "2026.10.2" ||
		a["app.model_version"].AsString() != "fraud-gbdt/1" || a["app.decision.score"].AsFloat64() < 0.61 ||
		strings.Join(a["app.decision.reason_codes"].AsStringSlice(), ",") != "NEW_DEVICE_AFTER_CREDENTIAL_CHANGE,MODEL_SCORE_ELEVATED" {
		t.Fatalf("decision attributes %v", a)
	}
	for _, k := range []attribute.Key{"app.account_id", "app.device_id", "enduser.id"} {
		if _, leaked := a[k]; leaked {
			t.Fatalf("identifier %s on the span", k)
		}
	}
	for _, child := range []string{"features.score_tx", "model.predict", "rules.evaluate", "postgres.commit"} {
		s, ok := byName[child]
		if !ok || s.Parent().SpanID() != score.SpanContext().SpanID() {
			t.Fatalf("missing child span %s of fraud.score", child)
		}
	}
	// The trace continues downstream: the persisted record carries the W3C traceparent for the outbox.
	stored := h.decisions.byTx[d.TransactionId]
	if !strings.Contains(stored.TraceParent, score.SpanContext().TraceID().String()) {
		t.Fatalf("traceparent %q does not carry trace %s", stored.TraceParent, score.SpanContext().TraceID())
	}
}

func TestDegradedScoreMarksTheSpan(t *testing.T) {
	h, rec, _ := traced(t)
	h.model.err = errors.New("model down")
	if _, err := h.srv.Score(context.Background(), &fraudv1.ScoreRequest{Transaction: journeyTx("0192f000-0000-7000-8000-0000000000e2")}); err != nil {
		t.Fatal(err)
	}
	for _, s := range rec.Ended() {
		switch s.Name() {
		case "fraud.score":
			if !attrs(s)["app.decision.degraded"].AsBool() {
				t.Fatal("degraded decision not marked on fraud.score")
			}
		case "model.predict":
			if s.Status().Code.String() != "Error" {
				t.Fatalf("model.predict status %v, want Error", s.Status())
			}
		}
	}
}

// A Score that fails to persist is an error on fraud.score itself, not only on its postgres.commit child.
func TestFailedCommitMarksTheScoreSpan(t *testing.T) {
	for _, commitErr := range []error{store.ErrOverloaded, errors.New("spool full")} {
		h, rec, _ := traced(t)
		h.decisions.err = commitErr
		if _, err := h.srv.Score(context.Background(), &fraudv1.ScoreRequest{Transaction: journeyTx("0192f000-0000-7000-8000-0000000000e3")}); err == nil {
			t.Fatal("expected an error")
		}
		found := false
		for _, s := range rec.Ended() {
			if s.Name() != "fraud.score" {
				continue
			}
			found = true
			if s.Status().Code.String() != "Error" {
				t.Fatalf("%v: fraud.score status %v, want Error", commitErr, s.Status())
			}
		}
		if !found {
			t.Fatalf("%v: no fraud.score span recorded", commitErr)
		}
	}
}

// When a concurrent duplicate won the race, the span describes the decision returned, not the one discarded.
func TestDuplicateScoreSpanDescribesTheReturnedDecision(t *testing.T) {
	h, rec, _ := traced(t)
	tx := journeyTx("0192f000-0000-7000-8000-0000000000e4")
	winner := &fraudv1.Decision{DecisionId: "0192f000-0000-7000-8000-0000000000aa", TransactionId: tx.TransactionId,
		Action: fraudv1.Action_ACTION_APPROVE, RulesVersion: "2026.10.2", ModelVersion: "fraud-gbdt/1"}
	h.decisions.byTx = map[string]store.DecisionRecord{tx.TransactionId: {Decision: winner, RequestHash: RequestHash(tx)}}
	d, err := h.srv.Score(context.Background(), &fraudv1.ScoreRequest{Transaction: tx})
	if err != nil || d.DecisionId != winner.DecisionId {
		t.Fatalf("%v %v", d, err)
	}
	found := false
	for _, s := range rec.Ended() {
		if s.Name() != "fraud.score" {
			continue
		}
		found = true
		a := attrs(s)
		if a["app.decision.id"].AsString() != winner.DecisionId || a["app.decision.action"].AsString() != "APPROVE" || !a["app.idempotent_replay"].AsBool() {
			t.Fatalf("span attributes %v describe the discarded decision", a)
		}
	}
	if !found {
		t.Fatal("no fraud.score span recorded")
	}
}
