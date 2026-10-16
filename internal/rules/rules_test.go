package rules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validYAML = `schema: fraud.rules.v1
version: "2026.10.1"
feature_spec: v1
thresholds: { review: 0.30, decline: 0.85 }
degraded:
  model_unavailable: { review_min_amount_minor: 20000 }
  features_unavailable: { review_min_amount_minor: 5000 }
model_reason_codes:
  - { min_score: 0.30, code: MODEL_SCORE_ELEVATED }
  - { min_score: 0.85, code: MODEL_SCORE_HIGH }
rules:
  - id: R001
    name: new_device_after_credential_change
    when: "new_device && min(minutes_since_pwd_change, minutes_since_email_change) < 60"
    action: REVIEW
    reason_code: NEW_DEVICE_AFTER_CREDENTIAL_CHANGE
  - id: R003
    name: card_testing_burst
    when: "tx_count_10m >= 8 && amount_minor <= 300"
    action: REVIEW
    reason_code: CARD_TESTING_BURST
  - id: R009
    name: blocked_mcc
    when: "mcc in ['7995']"
    action: DECLINE
    reason_code: BLOCKED_MCC
    final: true
`

func writeRules(t *testing.T, dir, version, body string) string {
	t.Helper()
	p := filepath.Join(dir, "rules.v"+version+".yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseAndEvaluate(t *testing.T) {
	rs, err := Parse([]byte(validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if rs.Version != "2026.10.1" || len(rs.Rules) != 3 || rs.Thresholds.Review != 0.30 {
		t.Fatalf("unexpected rule set %+v", rs)
	}
	env := Env{NewDevice: true, MinutesSincePwdChange: 43200, MinutesSinceEmailChange: 2, MCC: "5732", AmountMinor: 94000}
	m := rs.Evaluate(&env)
	if len(m) != 1 || m[0].ID != "R001" || m[0].Action != Review {
		t.Fatalf("matches %+v", m)
	}
	env = Env{TxCount10m: 9, AmountMinor: 150, MCC: "7995"}
	m = rs.Evaluate(&env)
	if len(m) != 2 || m[0].ID != "R003" || m[1].ID != "R009" || !m[1].Final || m[1].Action != Decline {
		t.Fatalf("matches %+v", m)
	}
}

func TestSchemaAndLimits(t *testing.T) {
	bad := []string{
		strings.Replace(validYAML, "fraud.rules.v1", "fraud.rules.v2", 1),
		strings.Replace(validYAML, "action: REVIEW", "action: BLOCK", 1),
		strings.Replace(validYAML, "id: R003", "id: R001", 1), // duplicate id
		strings.Replace(validYAML, "reason_code: CARD_TESTING_BURST", "reason_code: card testing", 1),
		strings.Replace(validYAML, `when: "mcc in ['7995']"`, `when: "amount_minor"`, 1), // not boolean
		strings.Replace(validYAML, `when: "mcc in ['7995']"`, `when: "unknown_feature > 1"`, 1),
	}
	for i, b := range bad {
		if _, err := Parse([]byte(b)); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
	var sb strings.Builder
	sb.WriteString(strings.Split(validYAML, "rules:\n")[0] + "rules:\n")
	for i := 0; i < 65; i++ {
		sb.WriteString("  - { id: R" + pad(i) + ", name: r, when: \"amount_minor > 1\", action: REVIEW, reason_code: TEST_CODE }\n")
	}
	if _, err := Parse([]byte(sb.String())); err == nil || !strings.Contains(err.Error(), "maxItems") {
		t.Fatalf("more than 64 rules must be rejected by maxItems, got %v", err)
	}
}

func pad(i int) string {
	s := "000" + itoa(i)
	return s[len(s)-3:]
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestInvalidExprKeepsPreviousVersion(t *testing.T) {
	dir := t.TempDir()
	writeRules(t, dir, "2026.10.1", validYAML)
	eng, err := NewEngine(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if eng.Active().Version != "2026.10.1" {
		t.Fatalf("active %s", eng.Active().Version)
	}
	broken := strings.Replace(strings.Replace(validYAML, "2026.10.1", "2026.10.2", 1), "tx_count_10m >= 8", "tx_count_10m >= ", 1)
	writeRules(t, dir, "2026.10.2", broken)
	if err := eng.Reload(); err == nil {
		t.Fatal("reload of an invalid expression must fail")
	}
	if eng.Active().Version != "2026.10.1" {
		t.Fatalf("invalid rule set replaced the active one: %s", eng.Active().Version)
	}
	fixed := strings.Replace(validYAML, "2026.10.1", "2026.10.2", 1)
	writeRules(t, dir, "2026.10.2", fixed)
	if err := eng.Reload(); err != nil {
		t.Fatal(err)
	}
	if eng.Active().Version != "2026.10.2" {
		t.Fatalf("active %s", eng.Active().Version)
	}
}

func TestWatchReloadsOnFileChange(t *testing.T) {
	dir := t.TempDir()
	writeRules(t, dir, "2026.10.1", validYAML)
	eng, err := NewEngine(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	go eng.Watch(ctx, 20*time.Millisecond, nil)
	writeRules(t, dir, "2026.10.3", strings.Replace(validYAML, "2026.10.1", "2026.10.3", 1))
	deadline := time.Now().Add(5 * time.Second)
	for eng.Active().Version != "2026.10.3" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if eng.Active().Version != "2026.10.3" {
		t.Fatalf("watch did not reload: %s", eng.Active().Version)
	}
}

func TestVersionOrdering(t *testing.T) {
	if !versionLess("2026.9.4", "2026.10.1") || versionLess("2026.10.10", "2026.10.9") {
		t.Fatal("versions must compare numerically")
	}
}

func TestRepositoryRuleSetIsValid(t *testing.T) {
	eng, err := NewEngine(filepath.Join("..", "..", "rules"), nil)
	if err != nil {
		t.Fatal(err)
	}
	rs := eng.Active()
	if rs.Version == "" || len(rs.Rules) == 0 || len(rs.Rules) > MaxRules {
		t.Fatalf("repository rule set %+v", rs)
	}
}

func BenchmarkEval64Rules(b *testing.B) {
	var sb strings.Builder
	sb.WriteString(strings.Split(validYAML, "rules:\n")[0] + "rules:\n")
	exprs := []string{
		"new_device && min(minutes_since_pwd_change, minutes_since_email_change) < 60",
		"tx_count_10m >= 8 && amount_minor <= 300",
		"channel == 'P2P' && distinct_devices_24h >= 3 && amount_minor >= 50000",
		"mcc in ['7995', '4829'] && amount_ratio_24h > 5",
	}
	for i := 0; i < MaxRules; i++ {
		sb.WriteString("  - { id: R" + pad(i) + ", name: r, when: \"" + exprs[i%len(exprs)] + "\", action: REVIEW, reason_code: TEST_CODE }\n")
	}
	rs, err := Parse([]byte(sb.String()))
	if err != nil {
		b.Fatal(err)
	}
	env := Env{NewDevice: true, MinutesSincePwdChange: 30, MinutesSinceEmailChange: 43200, TxCount10m: 3, AmountMinor: 900, Channel: "ECOMMERCE", MCC: "5732", AmountRatio24h: 2}
	b.ReportAllocs()
	b.ResetTimer()
	start := time.Now()
	for i := 0; i < b.N; i++ {
		_ = rs.Evaluate(&env)
	}
	per := time.Since(start) / time.Duration(b.N)
	b.ReportMetric(float64(per.Nanoseconds())/1000, "us/eval")
	if b.N > 100 && per > 50*time.Microsecond {
		b.Fatalf("evaluating 64 rules took %v (> 50µs)", per)
	}
}
