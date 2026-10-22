package decide

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"pgregory.net/rapid"

	"github.com/ROBERTG99/fraud-scoring-service/internal/rules"
)

func ruleSet(t testing.TB) *rules.RuleSet {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "rules", "rules.v2026.10.1.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	rs, err := rules.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

var (
	r001 = rules.Match{ID: "R001", ReasonCode: "NEW_DEVICE_AFTER_CREDENTIAL_CHANGE", Action: rules.Review}
	r003 = rules.Match{ID: "R003", ReasonCode: "CARD_TESTING_BURST", Action: rules.Review}
	r009 = rules.Match{ID: "R009", ReasonCode: "BLOCKED_MCC", Action: rules.Decline, Final: true}
)

// Rule R003 matches and the model scores 0.10 → REVIEW with CARD_TESTING_BURST.
func TestRuleR003WithLowScoreReviews(t *testing.T) {
	got := Combine(ruleSet(t), Input{Matches: []rules.Match{r003}, Score: 0.10, ModelOK: true, FeaturesOK: true, AmountMinor: 150})
	if got.Action != rules.Review || !reflect.DeepEqual(got.ReasonCodes, []string{"CARD_TESTING_BURST"}) || got.Degraded {
		t.Fatalf("got %+v", got)
	}
}

func TestFinalRuleWins(t *testing.T) {
	rs := ruleSet(t)
	// A final DECLINE wins even when the model says approve.
	got := Combine(rs, Input{Matches: []rules.Match{r001, r009}, Score: 0.01, ModelOK: true, FeaturesOK: true, AmountMinor: 1000})
	if got.Action != rules.Decline {
		t.Fatalf("got %+v", got)
	}
	want := []string{"BLOCKED_MCC", "NEW_DEVICE_AFTER_CREDENTIAL_CHANGE"}
	if !reflect.DeepEqual(got.ReasonCodes, want) {
		t.Fatalf("reason order %v, want final rule first then rules by id %v", got.ReasonCodes, want)
	}
	// A final rule also wins over a higher model band.
	approveFinal := rules.Match{ID: "R050", ReasonCode: "ALLOWLISTED_MERCHANT", Action: rules.Approve, Final: true}
	got = Combine(rs, Input{Matches: []rules.Match{approveFinal}, Score: 0.95, ModelOK: true, FeaturesOK: true})
	if got.Action != rules.Approve || got.ReasonCodes[0] != "ALLOWLISTED_MERCHANT" {
		t.Fatalf("final approve: %+v", got)
	}
}

func TestModelBandsAndReasonOrder(t *testing.T) {
	rs := ruleSet(t)
	cases := []struct {
		score   float64
		matches []rules.Match
		action  rules.Action
		reasons []string
	}{
		{0.29, nil, rules.Approve, nil},
		{0.30, nil, rules.Review, []string{"MODEL_SCORE_ELEVATED"}},
		{0.62, []rules.Match{r001}, rules.Review, []string{"NEW_DEVICE_AFTER_CREDENTIAL_CHANGE", "MODEL_SCORE_ELEVATED"}},
		{0.85, nil, rules.Decline, []string{"MODEL_SCORE_HIGH"}},
		{0.90, []rules.Match{r003, r001}, rules.Decline, []string{"NEW_DEVICE_AFTER_CREDENTIAL_CHANGE", "CARD_TESTING_BURST", "MODEL_SCORE_HIGH"}},
	}
	for _, c := range cases {
		got := Combine(rs, Input{Matches: c.matches, Score: c.score, ModelOK: true, FeaturesOK: true, AmountMinor: 1000})
		if got.Action != c.action || !reflect.DeepEqual(got.ReasonCodes, c.reasons) {
			t.Errorf("score %.2f: got %v %v, want %v %v", c.score, got.Action, got.ReasonCodes, c.action, c.reasons)
		}
	}
}

func TestDegradedBandByAmount(t *testing.T) {
	rs := ruleSet(t)
	cases := []struct {
		name      string
		modelOK   bool
		featureOK bool
		amount    int64
		action    rules.Action
		reasons   []string
	}{
		{"model down, small", false, true, 19_999, rules.Approve, []string{"MODEL_UNAVAILABLE"}},
		{"model down, at floor", false, true, 20_000, rules.Review, []string{"MODEL_UNAVAILABLE"}},
		{"features down, small", true, false, 4_999, rules.Approve, []string{"FEATURES_UNAVAILABLE"}},
		{"features down, at floor", true, false, 5_000, rules.Review, []string{"FEATURES_UNAVAILABLE"}},
		{"both down uses the lower floor", false, false, 5_000, rules.Review, []string{"MODEL_UNAVAILABLE", "FEATURES_UNAVAILABLE"}},
	}
	for _, c := range cases {
		// The model score is ignored in degraded mode, even if it is high.
		got := Combine(rs, Input{Score: 0.99, ModelOK: c.modelOK, FeaturesOK: c.featureOK, AmountMinor: c.amount})
		if got.Action != c.action || !reflect.DeepEqual(got.ReasonCodes, c.reasons) || !got.Degraded {
			t.Errorf("%s: got %+v", c.name, got)
		}
	}
	// Rules still apply in degraded mode, and their codes precede the degradation code.
	got := Combine(rs, Input{Matches: []rules.Match{r001}, ModelOK: false, FeaturesOK: true, AmountMinor: 100})
	if got.Action != rules.Review || !reflect.DeepEqual(got.ReasonCodes, []string{"NEW_DEVICE_AFTER_CREDENTIAL_CHANGE", "MODEL_UNAVAILABLE"}) {
		t.Fatalf("degraded with rule: %+v", got)
	}
}

func TestBandOf(t *testing.T) {
	rs := ruleSet(t)
	if BandOf(rs, 0.1) != "APPROVE" || BandOf(rs, 0.3) != "REVIEW" || BandOf(rs, 0.85) != "DECLINE" {
		t.Fatal("bands")
	}
}

// Property: for fixed rule matches, the action never decreases as the model score increases.
func TestActionMonotonicInScore(t *testing.T) {
	rs := ruleSet(t)
	all := []rules.Match{r001, r003, r009}
	rapid.Check(t, func(rt *rapid.T) {
		var ms []rules.Match
		for _, m := range all {
			if rapid.Bool().Draw(rt, "m"+m.ID) {
				ms = append(ms, m)
			}
		}
		a := rapid.Float64Range(0, 1).Draw(rt, "a")
		b := rapid.Float64Range(0, 1).Draw(rt, "b")
		if a > b {
			a, b = b, a
		}
		amount := rapid.Int64Range(1, 10_000_000).Draw(rt, "amount")
		lo := Combine(rs, Input{Matches: ms, Score: a, ModelOK: true, FeaturesOK: true, AmountMinor: amount})
		hi := Combine(rs, Input{Matches: ms, Score: b, ModelOK: true, FeaturesOK: true, AmountMinor: amount})
		if hi.Action < lo.Action {
			rt.Fatalf("score %.4f -> %v but %.4f -> %v", a, lo.Action, b, hi.Action)
		}
	})
}
