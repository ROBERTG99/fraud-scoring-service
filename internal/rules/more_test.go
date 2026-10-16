package rules

import (
	"testing"

	"github.com/ROBERTG99/fraud-scoring-service/internal/features"
)

func TestActionsAndEnv(t *testing.T) {
	for _, a := range []Action{Approve, Review, Decline} {
		got, err := ParseAction(a.String())
		if err != nil || got != a {
			t.Fatalf("round trip %v: %v %v", a, got, err)
		}
	}
	if _, err := ParseAction("BLOCK"); err == nil || Unspecified.String() != "UNSPECIFIED" {
		t.Fatal("unknown action")
	}
	var v features.Vector
	v[features.IdxNewDevice] = 1
	v[features.IdxMinutesSinceEmail] = 2
	v[features.IdxMinutesSincePwd] = 43200
	v[features.IdxAmountMinor] = 94000
	env := EnvFromVector(&v, "5732", "ECOMMERCE", "US", "USD")
	if !env.NewDevice || env.MCCNovel30d || env.AmountMinor != 94000 || env.Channel != "ECOMMERCE" {
		t.Fatalf("env %+v", env)
	}
	m := EnvFromMap(v.Map(), "5732", "ECOMMERCE", "US", "USD")
	if m != env {
		t.Fatalf("EnvFromMap %+v != %+v", m, env)
	}
	p, err := CompileExpr("new_device && min(minutes_since_pwd_change, minutes_since_email_change) < 60")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := EvalProgram(p, &env)
	if err != nil || !ok {
		t.Fatalf("EvalProgram %v %v", ok, err)
	}
	if _, err := CompileExpr("amount_minor +"); err == nil {
		t.Fatal("syntax error must fail")
	}
}

func TestNewEngineErrors(t *testing.T) {
	if _, err := NewEngine(t.TempDir(), nil); err == nil {
		t.Fatal("empty directory must fail")
	}
	if _, err := NewEngine("/nonexistent-rules-dir", nil); err == nil {
		t.Fatal("missing directory must fail")
	}
	dir := t.TempDir()
	writeRules(t, dir, "2026.10.1", "schema: [unterminated")
	if _, err := NewEngine(dir, nil); err == nil {
		t.Fatal("bad yaml must fail")
	}
}

func TestOnLoadCallback(t *testing.T) {
	dir := t.TempDir()
	writeRules(t, dir, "2026.10.1", validYAML)
	eng, err := NewEngine(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	eng.OnLoad(func(rs *RuleSet) { got = append(got, rs.Version) })
	if len(got) != 1 || got[0] != "2026.10.1" {
		t.Fatalf("OnLoad not called with the active set: %v", got)
	}
	// Reloading identical content is a no-op.
	if err := eng.Reload(); err != nil || len(got) != 1 {
		t.Fatalf("identical reload: %v %v", err, got)
	}
}

func TestThresholdOrderRejected(t *testing.T) {
	bad := `schema: fraud.rules.v1
version: "2026.10.1"
feature_spec: v1
thresholds: { review: 0.9, decline: 0.5 }
degraded:
  model_unavailable: { review_min_amount_minor: 20000 }
  features_unavailable: { review_min_amount_minor: 5000 }
model_reason_codes: [{ min_score: 0.3, code: MODEL_SCORE_ELEVATED }]
rules: [{ id: R001, name: r, when: "true", action: REVIEW, reason_code: TEST_CODE }]
`
	if _, err := Parse([]byte(bad)); err == nil {
		t.Fatal("review threshold above decline must fail")
	}
}
