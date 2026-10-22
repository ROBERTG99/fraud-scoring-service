package store

import (
	"strings"
	"testing"
	"time"
)

// Fields are length-prefixed: moving a separator between two fields changes the MAC (security audit). The
// unkeyed sha256 of '|'-joined fields could not tell these apart.
func TestAuditMACSeparatesFields(t *testing.T) {
	key := []byte(strings.Repeat("k", MinAuditKeyBytes))
	ts := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	a, _ := AuditMAC(key, "", ts, "analyst-1|analyst", "x", "case.label", "case/1", nil)
	b, _ := AuditMAC(key, "", ts, "analyst-1", "analyst|x", "case.label", "case/1", nil)
	if a == b {
		t.Fatal("moving '|' between fields kept the MAC")
	}
	ha, _ := AuditHash("", ts, "analyst-1|analyst", "x", "case.label", "case/1", nil)
	hb, _ := AuditHash("", ts, "analyst-1", "analyst|x", "case.label", "case/1", nil)
	if ha != hb {
		t.Fatal("expected the legacy hash to collide here (the reason for alg 2)")
	}
	other, _ := AuditMAC([]byte(strings.Repeat("o", MinAuditKeyBytes)), "", ts, "analyst-1|analyst", "x", "case.label", "case/1", nil)
	if other == a {
		t.Fatal("the MAC must depend on the key")
	}
}

func TestAuditAnchorsAreSignedAndParsed(t *testing.T) {
	key := []byte(strings.Repeat("k", MinAuditKeyBytes))
	a := NewAuditAnchor(key, AuditEntry{Seq: 7, Hash: "abc"}, time.Date(2026, 10, 18, 9, 0, 0, 0, time.FixedZone("x", 3600)), nil)
	if a.At.Location() != time.UTC || a.Prev != "" || a.MAC != anchorMAC(key, 7, "abc", a.At, "") {
		t.Fatalf("anchor %+v", a)
	}
	// Each anchor is chained to the one before it: removing or reordering a line breaks the chain.
	b := NewAuditAnchor(key, AuditEntry{Seq: 9, Hash: "def"}, a.At.Add(time.Hour), &a)
	c := NewAuditAnchor(key, AuditEntry{Seq: 12, Hash: "0a1"}, a.At.Add(2*time.Hour), &b)
	if err := CheckAnchorChain(key, []AuditAnchor{a, b, c}); err != nil {
		t.Fatal(err)
	}
	if err := CheckAnchorChain(key, []AuditAnchor{a, c}); err == nil || !strings.Contains(err.Error(), "chain") {
		t.Fatalf("a removed middle anchor must break the chain: %v", err)
	}
	if err := CheckAnchorChain(key, []AuditAnchor{b, c}); err == nil {
		t.Fatal("the first anchor must have no predecessor")
	}
	got, err := ReadAuditAnchors(strings.NewReader(`{"seq":7,"hash":"abc","at":"2026-10-18T08:00:00Z","prev":"","mac":"m"}` + "\n\n"))
	if err != nil || len(got) != 1 || got[0].Seq != 7 {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := ReadAuditAnchors(strings.NewReader("not json\n")); err == nil {
		t.Fatal("a malformed anchor line must fail")
	}
}
