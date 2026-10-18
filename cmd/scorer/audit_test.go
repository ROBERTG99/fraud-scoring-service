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
