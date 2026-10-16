package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/features"
	"github.com/ROBERTG99/fraud-scoring-service/internal/model"
	"github.com/ROBERTG99/fraud-scoring-service/internal/store"
)

func hasReason(d *fraudv1.Decision, code string) bool {
	for _, c := range d.ReasonCodes {
		if c == code {
			return true
		}
	}
	return false
}

// FR-8: the model server sleeps 50 ms → Score returns within 35 ms, rules-only, MODEL_UNAVAILABLE, model_version "none".
func TestModelTimeoutRulesOnlyWithin35ms(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(50 * time.Millisecond):
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte(`{"scores":[0.99],"model_version":"fraud-gbdt/1"}`))
	}))
	defer slow.Close()
	h := newHarness(t)
	h.srv.d.Model = model.NewBentoML(slow.URL, nil)

	// Warm the connection so the measurement covers the timeout path, not TCP setup.
	_, _ = h.srv.Score(context.Background(), &fraudv1.ScoreRequest{Transaction: journeyTx("0192f000-0000-7000-8000-0000000000a0")})

	for i, amount := range []int64{19_999, 20_000} {
		tx := journeyTx("0192f000-0000-7000-8000-00000000001" + string(rune('0'+i)))
		tx.AmountMinor = amount
		h.feats.vec = features.Vector{0, 1, 3, 4200, 0, 2, 0, 43200, 43200, 0, 2, 10, 1.0} // no rule fires
		start := time.Now()
		d, err := h.srv.Score(context.Background(), &fraudv1.ScoreRequest{Transaction: tx})
		elapsed := time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		if elapsed > 35*time.Millisecond {
			t.Fatalf("Score took %v with a 50 ms model (budget 35 ms)", elapsed)
		}
		if d.ModelVersion != "none" || d.Score != -1 || !d.Degraded || !hasReason(d, "MODEL_UNAVAILABLE") {
			t.Fatalf("decision %+v", d)
		}
		want := fraudv1.Action_ACTION_APPROVE
		if amount >= 20_000 {
			want = fraudv1.Action_ACTION_REVIEW
		}
		if d.Action != want {
			t.Fatalf("amount %d: action %v want %v (degraded band)", amount, d.Action, want)
		}
	}
}

// FR-8: Redis down → conservative defaults biased to REVIEW with FEATURES_UNAVAILABLE.
func TestRedisDownBiasesToReview(t *testing.T) {
	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 5 * time.Millisecond})
	defer func() { _ = dead.Close() }()
	h := newHarness(t)
	h.srv.d.Features = features.NewStore(dead, "t", nil)
	h.srv.d.Idempotency = &memIdem{}
	for _, c := range []struct {
		amount int64
		want   fraudv1.Action
	}{{4_999, fraudv1.Action_ACTION_APPROVE}, {5_000, fraudv1.Action_ACTION_REVIEW}, {19_000, fraudv1.Action_ACTION_REVIEW}} {
		tx := journeyTx(fmt.Sprintf("0192f000-0000-7000-8000-%012d", c.amount))
		tx.AmountMinor = c.amount
		start := time.Now()
		d, err := h.srv.Score(context.Background(), &fraudv1.ScoreRequest{Transaction: tx, Explain: true})
		if err != nil {
			t.Fatal(err)
		}
		if time.Since(start) > 45*time.Millisecond {
			t.Fatalf("Score exceeded its 45 ms deadline with Redis down: %v", time.Since(start))
		}
		if !d.Degraded || !hasReason(d, "FEATURES_UNAVAILABLE") || hasReason(d, "MODEL_UNAVAILABLE") || d.Action != c.want {
			t.Fatalf("amount %d: %+v", c.amount, d)
		}
		// The features used are the spec defaults (training p90) plus request fields.
		if d.Features["mcc_novel_30d"] != float64(features.DefaultSpec().Fallback["mcc_novel_30d"]) || d.Features["amount_minor"] != float64(c.amount) {
			t.Fatalf("fallback features %v", d.Features)
		}
	}
}

// A rule still applies in degraded mode (rules-only): blocked MCC declines without the model.
func TestRulesStillApplyWhenModelDown(t *testing.T) {
	h := newHarness(t)
	h.model.err = model.ErrUnavailable
	tx := journeyTx("0192f000-0000-7000-8000-0000000000c1")
	tx.Mcc = "7995"
	tx.AmountMinor = 100
	d, err := h.srv.Score(context.Background(), &fraudv1.ScoreRequest{Transaction: tx})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != fraudv1.Action_ACTION_DECLINE || d.ReasonCodes[0] != "BLOCKED_MCC" || !hasReason(d, "MODEL_UNAVAILABLE") {
		t.Fatalf("%+v", d)
	}
}

func TestWriterOverloadIsResourceExhausted(t *testing.T) {
	h := newHarness(t)
	h.decisions.err = store.ErrOverloaded
	_, err := h.srv.Score(context.Background(), &fraudv1.ScoreRequest{Transaction: journeyTx("0192f000-0000-7000-8000-0000000000d1")})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("want ResourceExhausted, got %v", err)
	}
	h.decisions.err = errors.New("disk full")
	_, err = h.srv.Score(context.Background(), &fraudv1.ScoreRequest{Transaction: journeyTx("0192f000-0000-7000-8000-0000000000d2")})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("want Unavailable when even the spool fails, got %v", err)
	}
}
