package rules

import (
	"strings"
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

func TestBuiltinsRestricted(t *testing.T) {
	for _, bad := range []string{"now() != nil", "len(mcc) > 3", "upper(mcc) == '5732'"} {
		if _, err := CompileExpr(bad); err == nil {
			t.Errorf("%q compiled; builtins other than min/max/abs must be disabled", bad)
		}
	}
	for _, ok := range []string{"min(tx_count_1h, 3) > 1", "max(amount_minor, 1) > 0", "abs(amount_ratio_24h - 1) > 0.5"} {
		if _, err := CompileExpr(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
}

func TestFileVersionMustMatchYAML(t *testing.T) {
	dir := t.TempDir()
	writeRules(t, dir, "2026.10.1", validYAML)
	eng, err := NewEngine(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeRules(t, dir, "2026.10.9", validYAML) // file says 2026.10.9, YAML says 2026.10.1
	if err := eng.Reload(); err == nil || eng.Active().Version != "2026.10.1" {
		t.Fatalf("mismatched version accepted: %v", err)
	}
}

func TestReloadRejectsOtherFeatureSpec(t *testing.T) {
	dir := t.TempDir()
	writeRules(t, dir, "2026.10.1", validYAML) // feature_spec v1
	eng, err := NewEngine(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	eng.RequireFeatureSpec("v1")
	v2 := strings.Replace(strings.Replace(validYAML, `"2026.10.1"`, `"2026.10.2"`, 1), "feature_spec: v1", "feature_spec: v2", 1)
	writeRules(t, dir, "2026.10.2", v2)
	if err := eng.Reload(); err == nil || !strings.Contains(err.Error(), "feature spec v2") {
		t.Fatalf("reload of a v2 rule set into a v1 scorer: %v", err)
	}
	if eng.Active().Version != "2026.10.1" {
		t.Fatalf("active set changed to %s", eng.Active().Version)
	}
}

// TestCompileExprRejectsUnboundedWork: security audit M6 H1. A 60-character expression with nested
// predicates over ranges ran for seconds per sample inside the scorer; rules only ever need comparisons,
// boolean logic, membership and min/max/abs over the env.
func TestCompileExprRejectsUnboundedWork(t *testing.T) {
	for _, bad := range []string{
		"all(1..990, {all(1..990, {# > 0 && # < 2000 && # * # > 0})})",
		"any(1..10, {# > 5})",
		"count([1, 2, 3], {# > 1}) > 0",
		"len(filter([1, 2], {# > 0})) > 0",
		"1..1000000 == []",
		"let x = 1; x > 0",
		"amount_minor > 0 " + strings.Repeat("&& amount_minor > 0 ", 120),
	} {
		if _, err := CompileExpr(bad); err == nil {
			t.Errorf("compiled %q, want rejected", bad)
		}
	}
	for _, good := range []string{
		"new_device && min(minutes_since_pwd_change, minutes_since_email_change) < 60",
		"mcc in ['7995'] || channel == 'P2P' && abs(amount_ratio_24h) >= 8",
		"tx_count_10m >= 8 ? amount_minor <= 300 : false",
		"max(tx_count_1h, tx_count_10m) >= 20 && not new_device",
	} {
		if _, err := CompileExpr(good); err != nil {
			t.Errorf("rejected %q: %v", good, err)
		}
	}
}
