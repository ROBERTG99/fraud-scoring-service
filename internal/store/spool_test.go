package store

import (
	"testing"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
)

// T-48: a scorer killed while Postgres was down leaves its fsync'd decisions in spool.jsonl. The restarted
// process never opened that file, and its Drain must still replay it (TestDurabilityKill lost them before).
func TestDrainReplaysTheSpoolOfAProcessThatDied(t *testing.T) {
	dir := t.TempDir()
	dead, err := OpenSpool(dir)
	if err != nil {
		t.Fatal(err)
	}
	recs := []DecisionRecord{
		{Decision: &fraudv1.Decision{DecisionId: "0192f000-0000-7000-9000-000000000001", TransactionId: "0192f000-0000-7000-8000-000000000001"}, AccountID: "acc_1"},
		{Decision: &fraudv1.Decision{DecisionId: "0192f000-0000-7000-9000-000000000002", TransactionId: "0192f000-0000-7000-8000-000000000002"}, AccountID: "acc_2"},
	}
	if err := dead.Append(recs); err != nil {
		t.Fatal(err)
	}
	// The process is killed: no Close, no Drain. A new process opens the same directory.
	restarted, err := OpenSpool(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restarted.Close() }()
	if got := restarted.Depth(); got != 2 {
		t.Fatalf("depth after restart %d, want 2", got)
	}
	var replayed []string
	n, err := restarted.Drain(func(rs []DecisionRecord) error {
		for _, r := range rs {
			replayed = append(replayed, r.Decision.DecisionId)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || len(replayed) != 2 || restarted.Depth() != 0 {
		t.Fatalf("replayed %d (%v), depth %d; want both records and depth 0", n, replayed, restarted.Depth())
	}
	// Nothing is replayed twice.
	if n, err := restarted.Drain(func([]DecisionRecord) error { return nil }); err != nil || n != 0 {
		t.Fatalf("second drain replayed %d (%v), want 0", n, err)
	}
}
