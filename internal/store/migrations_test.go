//go:build integration

package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

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
		"shadow_comparisons", "rule_sets", "model_versions", "step_records", "processed_messages", "audit_log", "account_events"} {
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
	if _, err := owner.Exec(ctx, `INSERT INTO audit_log (actor_sub, role, action, target, details, prev_hash, hash)
		VALUES ('analyst-1', 'analyst', 'case.label', 'case/1', '{}', '', 'h1')`); err != nil {
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
	if _, err := app.Exec(ctx, `INSERT INTO audit_log (actor_sub, role, action, target, details, prev_hash, hash)
		VALUES ('analyst-1', 'analyst', 'case.label', 'case/2', '{}', 'h1', 'h2')`); err != nil {
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
