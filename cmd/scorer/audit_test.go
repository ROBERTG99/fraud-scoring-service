package main

import (
	"strings"
	"testing"
)

func TestAuditKeyFromEnv(t *testing.T) {
	t.Setenv("FRAUD_ENV", "")
	t.Setenv("FRAUD_AUDIT_KEY", "")
	if _, err := auditKeyFromEnv(); err == nil {
		t.Fatal("outside dev mode a missing key must stop the scorer")
	}
	t.Setenv("FRAUD_AUDIT_KEY", strings.Repeat("ab", 16))
	if _, err := auditKeyFromEnv(); err == nil {
		t.Fatal("a 16-byte key must be refused")
	}
	t.Setenv("FRAUD_AUDIT_KEY", "not hex")
	if _, err := auditKeyFromEnv(); err == nil {
		t.Fatal("a key that is not hex must be refused")
	}
	t.Setenv("FRAUD_AUDIT_KEY", strings.Repeat("ab", 32))
	if k, err := auditKeyFromEnv(); err != nil || len(k) != 32 || k[0] != 0xab {
		t.Fatalf("%x %v", k, err)
	}
	t.Setenv("FRAUD_AUDIT_KEY", "")
	t.Setenv("FRAUD_ENV", "dev")
	if k, err := auditKeyFromEnv(); err != nil || len(k) != 32 {
		t.Fatalf("dev key: %x %v", k, err)
	}
}

// Outside dev mode, verification without anchors cannot see removed entries, so it refuses to run (M6 round 2).
func TestAuditVerifyNeedsAnchorsOutsideDev(t *testing.T) {
	t.Setenv("FRAUD_ENV", "")
	t.Setenv("FRAUD_AUDIT_KEY", strings.Repeat("ab", 32))
	t.Setenv("FRAUD_AUDIT_ANCHORS", "")
	if code := auditVerify(false); code != 2 {
		t.Fatalf("audit-verify without anchors outside dev: exit %d, want 2", code)
	}
	t.Setenv("FRAUD_AUDIT_LEGACY_MAX_SEQ", "-1")
	t.Setenv("FRAUD_ENV", "dev")
	if code := auditVerify(false); code != 2 {
		t.Fatalf("a negative legacy bound: exit %d, want 2", code)
	}
}

// Lifting the event_ts age bound is for replaying historical data on a local stack only (round 2, H2).
func TestEventAgeBoundLiftsOnlyInDevMode(t *testing.T) {
	t.Setenv("FRAUD_ENV", "")
	if _, err := eventAge("0"); err == nil {
		t.Fatal("FRAUD_MAX_EVENT_AGE=0 accepted outside dev")
	}
	if d, err := eventAge("2h"); err != nil || d.Hours() != 2 {
		t.Fatalf("%v %v", d, err)
	}
	t.Setenv("FRAUD_ENV", "dev")
	if d, err := eventAge("0"); err != nil || d != -1 {
		t.Fatalf("%v %v", d, err)
	}
}

// Outside dev mode an anchors file with no anchor in it cannot vouch for a non-empty chain (round 3 review).
func TestAuditVerifyRefusesAnEmptyAnchorsFileOutsideDev(t *testing.T) {
	t.Setenv("FRAUD_ENV", "")
	if err := requireAnchors(false, nil, 3); err == nil {
		t.Fatal("an empty anchors file vouched for 3 entries outside dev")
	}
	if err := requireAnchors(false, nil, 0); err != nil {
		t.Fatalf("an empty chain needs no anchor yet: %v", err)
	}
	if err := requireAnchors(true, nil, 3); err != nil {
		t.Fatalf("audit-anchor writes the first anchor: %v", err)
	}
	t.Setenv("FRAUD_ENV", "dev")
	if err := requireAnchors(false, nil, 3); err != nil {
		t.Fatalf("dev mode: %v", err)
	}
}
