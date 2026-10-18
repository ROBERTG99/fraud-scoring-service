//go:build integration

package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/testutil"
)

func TestUpDownUp(t *testing.T) {
	dsn := testutil.NewDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := MigrateUp(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	v1, err := MigrationVersion(ctx, dsn)
	if err != nil || v1 < 4 {
		t.Fatalf("version after up = %d, %v", v1, err)
	}
	if err := MigrateDownAll(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	if v, _ := MigrationVersion(ctx, dsn); v != 0 {
		t.Fatalf("version after down = %d", v)
	}
	if err := MigrateUp(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	for _, table := range []string{"decisions", "decision_outbox", "review_cases", "case_summaries", "analyst_labels",
		"shadow_comparisons", "rule_sets", "model_versions", "step_records", "processed_messages", "audit_log", "account_events", "summary_attempts"} {
		var n int
		if err := conn.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Errorf("table %s: %v", table, err)
		}
	}
}

func TestAuditLogRejectsUpdate(t *testing.T) {
	dsn := testutil.NewDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := MigrateUp(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	owner, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close(ctx) }()
	if _, err := owner.Exec(ctx, `INSERT INTO audit_log (actor_sub, role, action, target, details, prev_hash, hash, alg)
		VALUES ('analyst-1', 'analyst', 'case.label', 'case/1', '{}', '', 'h1', 2)`); err != nil {
		t.Fatal(err)
	}
	// The trigger rejects changes even for the table owner.
	for _, stmt := range []string{"UPDATE audit_log SET actor_sub = 'x'", "DELETE FROM audit_log", "TRUNCATE audit_log"} {
		_, err := owner.Exec(ctx, stmt)
		if err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s as owner: want append-only error, got %v", stmt, err)
		}
	}
	// fraud_app has no UPDATE/DELETE privilege at all, but can INSERT.
	app, err := pgx.Connect(ctx, testutil.WithUser(t, dsn, "fraud_app", "fraud_app"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = app.Close(ctx) }()
	if _, err := app.Exec(ctx, `INSERT INTO audit_log (actor_sub, role, action, target, details, prev_hash, hash, alg)
		VALUES ('analyst-1', 'analyst', 'case.label', 'case/2', '{}', 'h1', 'h2', 2)`); err != nil {
		t.Fatalf("fraud_app insert: %v", err)
	}
	for _, stmt := range []string{"UPDATE audit_log SET actor_sub = 'x'", "DELETE FROM audit_log"} {
		_, err := app.Exec(ctx, stmt)
		if err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s as fraud_app: want permission denied, got %v", stmt, err)
		}
	}
	var n int
	if err := owner.QueryRow(ctx, "SELECT count(*) FROM audit_log").Scan(&n); err != nil || n != 2 {
		t.Fatalf("audit rows = %d, %v", n, err)
	}
}

func TestDecisionConstraints(t *testing.T) {
	dsn := testutil.NewDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := MigrateUp(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	_, err = conn.Exec(ctx, `INSERT INTO decisions VALUES (gen_random_uuid(), 'tx', 'acc', 'BLOCK', NULL, '{}', 'none', 'v', true, '{}', 1, now(), '{}')`)
	if err == nil {
		t.Fatal("action outside APPROVE|REVIEW|DECLINE must be rejected")
	}
	if _, err := conn.Exec(ctx, `INSERT INTO rule_sets VALUES ('a', 's', '{}', now(), true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO rule_sets VALUES ('b', 's', '{}', now(), true)`); err == nil {
		t.Fatal("only one active rule set allowed")
	}
}

// TestServiceRolesAreLeastPrivilege: security audit M6 M1. Each service has its own role; no service role can
// rewrite a decision or a label, the summarizer writes only its own tables, and only retention deletes.
func TestServiceRolesAreLeastPrivilege(t *testing.T) {
	dsn := migratedDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := newPool(t, dsn)
	w, err := NewGroupWriter(pool, relayWriterConfig(t), quiet)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(ctx, record(6001, fraudv1.Action_ACTION_REVIEW)); err != nil {
		t.Fatal(err)
	}
	w.Close()
	connect := func(role string) *pgx.Conn {
		c, err := pgx.Connect(ctx, testutil.WithUser(t, dsn, role, role))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close(context.Background()) })
		return c
	}
	try := func(c *pgx.Conn, stmt string) error {
		_, err := c.Exec(ctx, stmt)
		return err
	}
	denied := func(role, stmt string) {
		if err := try(connect(role), stmt); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s: %q want permission denied, got %v", role, stmt, err)
		}
	}
	allowed := func(role, stmt string) {
		if err := try(connect(role), stmt); err != nil {
			t.Errorf("%s: %q: %v", role, stmt, err)
		}
	}
	caseID := `(SELECT case_id FROM review_cases LIMIT 1)`
	for _, role := range []string{"fraud_app", "fraud_summarizer", "fraud_retention"} {
		denied(role, `UPDATE decisions SET action = 'APPROVE'`)
		denied(role, `UPDATE analyst_labels SET label = 'legit'`)
	}
	denied("fraud_app", `DELETE FROM decisions`)
	denied("fraud_app", `DELETE FROM review_cases`)
	allowed("fraud_app", `UPDATE decision_outbox SET attempts = attempts + 1`)
	denied("fraud_summarizer", `INSERT INTO decisions (decision_id) VALUES (gen_random_uuid())`)
	denied("fraud_summarizer", `UPDATE review_cases SET status = 'labelled'`)
	denied("fraud_summarizer", `DELETE FROM case_summaries`)
	allowed("fraud_summarizer", `SELECT count(*) FROM decisions d JOIN review_cases r USING (decision_id)`)
	allowed("fraud_summarizer", `INSERT INTO case_summaries (case_id, status, summary, pattern, faithfulness, model, prompt_version, cost_usd)
	  VALUES (`+caseID+`, 'ok', '{}', 'unclear', 1, 'none', 'v', 0)
	  ON CONFLICT (case_id) DO UPDATE SET status = EXCLUDED.status`)
	allowed("fraud_summarizer", `INSERT INTO processed_messages (consumer, message_key) VALUES ('s', 'k')`)
	allowed("fraud_summarizer", `INSERT INTO summary_attempts (consumer, message_key) VALUES ('s', 'k')
	  ON CONFLICT (consumer, message_key) DO UPDATE SET attempts = summary_attempts.attempts + 1`)
	denied("fraud_summarizer", `DELETE FROM summary_attempts`)
	allowed("fraud_summarizer", `INSERT INTO summary_regenerations (case_id, idempotency_key, subject) VALUES (`+caseID+`, 'k', 's')`)
	denied("fraud_summarizer", `DELETE FROM summary_regenerations`)
	denied("fraud_app", `INSERT INTO summary_attempts (consumer, message_key) VALUES ('a', 'k')`)
	denied("fraud_retention", `INSERT INTO decisions (decision_id) VALUES (gen_random_uuid())`)
	allowed("fraud_retention", `DELETE FROM decisions`) // cascades to the case, summary and outbox rows
	if n := count(t, pool, "SELECT count(*) FROM review_cases"); n != 0 {
		t.Fatalf("cascade as fraud_retention left %d cases", n)
	}
}

// Retention deletes decisions and the outbox rows cascade: the foreign key needs an index, or every deleted
// decision scans decision_outbox (M6 review).
func TestOutboxDecisionIDIsIndexed(t *testing.T) {
	pool := newPool(t, migratedDB(t))
	n := count(t, pool, `SELECT count(*) FROM pg_indexes WHERE tablename = 'decision_outbox' AND indexdef LIKE '%(decision_id)%'`)
	if n != 1 {
		t.Fatalf("decision_outbox(decision_id) indexes: %d, want 1", n)
	}
}
