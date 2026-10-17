//go:build integration

package store

import (
	"context"
	"testing"
	"time"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/testutil"
)

// TestRetentionDeletesOnlyExpiredRows: spec §4.5 retention (400 days decisions and their cases, summaries,
// labels and shadow rows; 7 days sent outbox rows; 90 days step records and processed messages), never the
// audit log, never an unsent outbox row of a kept decision.
func TestRetentionDeletesOnlyExpiredRows(t *testing.T) {
	dsn := migratedDB(t)
	pool := newPool(t, dsn)
	app := newPool(t, testutil.WithUser(t, dsn, "fraud_app", "fraud_app")) // the role cmd/retention runs as
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	w, err := NewGroupWriter(pool, relayWriterConfig(t), quiet)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ { // 1, 2 old; 3, 4 recent. All REVIEW, so each has a case.
		if _, err := w.Commit(ctx, record(5000+i, fraudv1.Action_ACTION_REVIEW)); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()
	now := time.Date(2027, 11, 1, 0, 0, 0, 0, time.UTC)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	old, recent := now.AddDate(0, 0, -401), now.AddDate(0, 0, -30)
	exec(`UPDATE decisions SET decided_at = CASE WHEN transaction_id IN ($1, $2) THEN $3::timestamptz ELSE $4::timestamptz END`,
		record(5001, 0).Decision.TransactionId, record(5002, 0).Decision.TransactionId, old, recent)
	exec(`INSERT INTO case_summaries (case_id, status, summary, pattern, faithfulness, model, prompt_version, cost_usd, created_at)
	      SELECT case_id, 'ok', '{}', 'unclear', 1, 'none', 'v', 0, $1 FROM review_cases`, recent)
	exec(`INSERT INTO shadow_comparisons (decision_id, primary_version, shadow_version, primary_score, shadow_score, primary_band, shadow_band, agree, created_at)
	      SELECT decision_id, 'p', 's', 0.5, 0.5, 'REVIEW', 'REVIEW', true, $1 FROM decisions`, recent)
	// Outbox: decision 3 sent 8 days ago (expired), decision 4 sent yesterday; decision 1 unsent (goes with its decision).
	exec(`UPDATE decision_outbox SET sent_at = CASE
	        WHEN decision_id = $1 THEN $3::timestamptz WHEN decision_id = $2 THEN $4::timestamptz END`,
		record(5003, 0).Decision.DecisionId, record(5004, 0).Decision.DecisionId, now.AddDate(0, 0, -8), now.AddDate(0, 0, -1))
	exec(`INSERT INTO step_records (run_id, step, node, kind, input_tokens, output_tokens, cost_usd, latency_ms, status, started_at)
	      VALUES ('old', 1, 'n', 'compute', 0, 0, 0, 0, 'ok', $1), ('new', 1, 'n', 'compute', 0, 0, 0, 0, 'ok', $2)`,
		now.AddDate(0, 0, -91), now.AddDate(0, 0, -89))
	exec(`INSERT INTO processed_messages (consumer, message_key, processed_at) VALUES ('c', 'old', $1), ('c', 'new', $2)`,
		now.AddDate(0, 0, -91), now.AddDate(0, 0, -89))
	exec(`INSERT INTO account_events (event_id, account_id, kind, event_ts, payload) VALUES ('old', 'acc_00000042', 'login', $1, '{}'), ('new', 'acc_00000042', 'login', $2, '{}')`,
		old, recent)
	audit := count(t, pool, "SELECT count(*) FROM audit_log")

	got, err := RunRetention(ctx, app, now, DefaultRetention(), 1) // batch size 1 exercises the loop
	if err != nil {
		t.Fatal(err)
	}
	want := RetentionResult{Decisions: 2, OutboxSent: 1, StepRecords: 1, ProcessedMessages: 1, AccountEvents: 1}
	if got != want {
		t.Fatalf("deleted %+v, want %+v", got, want)
	}
	for q, n := range map[string]int{
		"SELECT count(*) FROM decisions":                                       2,
		"SELECT count(*) FROM review_cases":                                    2, // cascaded with their decisions
		"SELECT count(*) FROM case_summaries":                                  2,
		"SELECT count(*) FROM shadow_comparisons":                              2,
		"SELECT count(*) FROM decision_outbox":                                 1, // decision 4's recently sent row
		"SELECT count(*) FROM step_records":                                    1,
		"SELECT count(*) FROM processed_messages":                              1,
		"SELECT count(*) FROM account_events WHERE event_id IN ('old', 'new')": 1,
		"SELECT count(*) FROM audit_log":                                       audit,
	} {
		if c := count(t, pool, q); c != n {
			t.Errorf("%s = %d, want %d", q, c, n)
		}
	}
	if again, err := RunRetention(ctx, app, now, DefaultRetention(), 1000); err != nil || again != (RetentionResult{}) {
		t.Fatalf("second run deleted %+v (%v), want nothing", again, err)
	}
}
