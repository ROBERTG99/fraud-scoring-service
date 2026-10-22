//go:build integration

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ROBERTG99/fraud-scoring-service/internal/store"
	"github.com/ROBERTG99/fraud-scoring-service/internal/testutil"
)

// audit-verify outside dev mode, on a chain with entries, refuses an anchors file that holds no anchor; once
// audit-anchor has written one it verifies (review round 4: requireAnchors wired into auditVerify).
func TestAuditVerifyRefusesAChainWithNoAnchorOutsideDev(t *testing.T) {
	admin := testutil.NewDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := store.MigrateUp(ctx, admin); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	// A role with a password that is not its name, so the DSN check passes outside dev mode.
	role, password := testutil.UniqueName("audit_"), testutil.UniqueName("pw_")
	if _, err := conn.Exec(ctx, "CREATE ROLE "+role+" LOGIN PASSWORD '"+password+"' IN ROLE fraud_app"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if c, err := pgx.Connect(ctx, testutil.PostgresAdminDSN(t)); err == nil {
			_, _ = c.Exec(ctx, "DROP ROLE IF EXISTS "+role)
			_ = c.Close(ctx)
		}
	})
	t.Setenv("FRAUD_ENV", "")
	t.Setenv("FRAUD_AUDIT_KEY", strings.Repeat("cd", 32))
	key, err := auditKeyFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendAudit(ctx, tx, key, "analyst-1", "analyst", "case.label", "case/1", map[string]any{"label": "fraud"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	anchors := filepath.Join(t.TempDir(), "anchors.jsonl")
	if err := os.WriteFile(anchors, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FRAUD_AUDIT_ANCHORS", anchors)
	t.Setenv("FRAUD_PG_DSN", testutil.WithUser(t, admin, role, password))
	stderr := captureStderr(t, func() {
		if code := auditVerify(false); code != 2 {
			t.Errorf("audit-verify with an empty anchors file: exit %d, want 2", code)
		}
	})
	if !strings.Contains(stderr, "holds no anchor") {
		t.Fatalf("stderr %q does not name the missing anchor", stderr)
	}
	if code := auditVerify(true); code != 0 {
		t.Fatalf("audit-anchor: exit %d", code)
	}
	if code := auditVerify(false); code != 0 {
		t.Fatalf("audit-verify after audit-anchor: exit %d", code)
	}
}

func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	done := make(chan string)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				done <- b.String()
				return
			}
		}
	}()
	f()
	os.Stderr = old
	_ = w.Close()
	return <-done
}
