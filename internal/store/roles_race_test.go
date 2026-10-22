//go:build integration

package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ROBERTG99/fraud-scoring-service/internal/testutil"
)

var roleBlock = regexp.MustCompile(`(?s)-- \+goose StatementBegin\n(DO \$\$.*?\$\$;)\n-- \+goose StatementEnd`)

// Roles are cluster-wide: on a fresh server, databases migrated in parallel (the Go test packages do this)
// both found a role missing and both created it; the loser failed with a unique violation (fresh-clone
// run). The migrations' own role blocks, renamed to fresh roles, must succeed when run concurrently.
func TestRoleMigrationsTolerateConcurrentRuns(t *testing.T) {
	admin := testutil.PostgresAdminDSN(t)
	root := testutil.RepoRoot()
	for _, file := range []string{"00001_roles.sql", "00007_service_roles.sql"} {
		body, err := os.ReadFile(filepath.Join(root, "migrations", file)) // #nosec G304 -- repository file
		if err != nil {
			t.Fatal(err)
		}
		m := roleBlock.FindSubmatch(body)
		if m == nil {
			t.Fatalf("%s: no role block", file)
		}
		suffix := strings.ToLower(testutil.UniqueName("r"))
		block := regexp.MustCompile(`\b(fraud_[a-z]+)\b`).ReplaceAllString(string(m[1]), "${1}_"+suffix)
		var created []string
		for _, r := range regexp.MustCompile(`CREATE ROLE (\w+)`).FindAllStringSubmatch(block, -1) {
			created = append(created, r[1])
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if c, err := pgx.Connect(ctx, admin); err == nil {
				for _, r := range created {
					_, _ = c.Exec(ctx, "DROP ROLE IF EXISTS "+r)
				}
				_ = c.Close(ctx)
			}
		})
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		start := make(chan struct{})
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				c, err := pgx.Connect(ctx, admin)
				if err != nil {
					errs <- err
					return
				}
				defer func() { _ = c.Close(context.Background()) }()
				<-start
				if _, err := c.Exec(ctx, block); err != nil {
					errs <- fmt.Errorf("%s: %w", file, err)
				}
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
	}
}
