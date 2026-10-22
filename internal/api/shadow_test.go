package api

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/decide"
	"github.com/ROBERTG99/fraud-scoring-service/internal/features"
	"github.com/ROBERTG99/fraud-scoring-service/internal/model"
)

// aliasModel answers production normally and fails every shadow call.
type aliasModel struct{ primary *fakeModel }

func (a aliasModel) Predict(ctx context.Context, alias string, rows [][]float32) (model.Prediction, error) {
	if alias == model.AliasShadow {
		return model.Prediction{}, errors.New("shadow backend down")
	}
	p, err := a.primary.Predict(ctx, alias, rows)
	if err == nil {
		// Vary the score with the input so the 1,000 decisions cover every band.
		p.Scores = []float32{float32(int(rows[0][features.IdxAmountMinor])%100) / 100}
	}
	return p, err
}

func strip(d *fraudv1.Decision) *fraudv1.Decision {
	return &fraudv1.Decision{TransactionId: d.TransactionId, Action: d.Action, Score: d.Score, ReasonCodes: d.ReasonCodes,
		ModelVersion: d.ModelVersion, RulesVersion: d.RulesVersion, Degraded: d.Degraded}
}

// With the shadow backend failing on every call, 1,000 decisions equal the run with shadow disabled.
func TestShadowFailureDoesNotChangeDecisions(t *testing.T) {
	run := func(withShadow bool) ([]*fraudv1.Decision, *model.ShadowRunner) {
		h := newHarness(t)
		h.feats.vec = features.Vector{0, 1, 3, 4200, 0, 1, 0, 43200, 43200, 0, 2, 10, 1.0} // no rule fires by default
		m := aliasModel{primary: h.model}
		h.srv.d.Model = m
		var runner *model.ShadowRunner
		if withShadow {
			rs := h.srv.d.Rules.Active()
			runner = model.NewShadowRunner(m, nil, func(s float64) string { return decide.BandOf(rs, s) }, model.ShadowConfig{}, nil)
			h.srv.d.Shadow = runner
		}
		out := make([]*fraudv1.Decision, 0, 1000)
		for i := 0; i < 1000; i++ {
			tx := journeyTx(fmt.Sprintf("0192f000-0000-7000-8000-%012d", 10_000+i))
			tx.AmountMinor = int64(100 + i*997%500_000)
			if i%10 == 0 {
				tx.Mcc = "7995"
			}
			d, err := h.srv.Score(context.Background(), &fraudv1.ScoreRequest{Transaction: tx})
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, strip(d))
		}
		if runner != nil {
			runner.Close()
		}
		return out, runner
	}
	without, _ := run(false)
	with, runner := run(true)
	actions := map[fraudv1.Action]int{}
	for i := range without {
		if !reflect.DeepEqual(without[i], with[i]) {
			t.Fatalf("decision %d differs with a failing shadow:\n without %+v\n with    %+v", i, without[i], with[i])
		}
		actions[with[i].Action]++
	}
	if len(actions) < 3 {
		t.Fatalf("test should cover every action, got %v", actions)
	}
	done, failed, dropped := runner.Stats()
	if done != 0 || failed+dropped != 1000 {
		t.Fatalf("shadow stats done=%d failed=%d dropped=%d", done, failed, dropped)
	}
}
