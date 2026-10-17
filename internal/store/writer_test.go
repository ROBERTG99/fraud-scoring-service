//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/events"
	"github.com/ROBERTG99/fraud-scoring-service/internal/testutil"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func migratedDB(t *testing.T) string {
	t.Helper()
	dsn := testutil.NewDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := MigrateUp(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	return dsn
}

func newPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 8
	cfg.ConnConfig.ConnectTimeout = 200 * time.Millisecond
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func record(i int, action fraudv1.Action) DecisionRecord {
	tx := fmt.Sprintf("0192f000-0000-7000-8000-%012d", i)
	d := &fraudv1.Decision{DecisionId: fmt.Sprintf("0192f000-0000-7000-9000-%012d", i), TransactionId: tx, Action: action, Score: 0.62,
		ReasonCodes: []string{"NEW_DEVICE_AFTER_CREDENTIAL_CHANGE", "MODEL_SCORE_ELEVATED"}, ModelVersion: "fraud-gbdt/1", RulesVersion: "2026.10.1",
		LatencyMs: 4.2, DecidedAt: timestamppb.New(time.Date(2026, 9, 20, 10, 3, 0, 0, time.UTC))}
	return DecisionRecord{Decision: d, AccountID: "acc_00000042", Features: map[string]float64{"amount_minor": 94000, "new_device": 1}, AmountMinor: 94000, Currency: "USD",
		Tx:    TxAttrs{AmountMinor: 94000, Currency: "USD", MerchantID: "m_00042", MCC: "5732", Channel: "ECOMMERCE", Country: "US"},
		Event: &events.Event{Kind: events.KindTransaction, EventID: tx, AccountID: "acc_00000042", TSMillis: d.DecidedAt.AsTime().UnixMilli()}}
}

func count(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCommitBeforeRespond(t *testing.T) {
	dsn := migratedDB(t)
	pool := newPool(t, dsn)
	w, err := NewGroupWriter(pool, WriterConfig{SpoolDir: t.TempDir()}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond*20)
	defer cancel()
	review := record(1, fraudv1.Action_ACTION_REVIEW)
	got, err := w.Commit(ctx, review)
	if err != nil {
		t.Fatal(err)
	}
	if got.DecisionId != review.Decision.DecisionId {
		t.Fatalf("returned %s", got.DecisionId)
	}
	// Commit has returned: the decision, its outbox row and its review case are already durable.
	if n := count(t, pool, "SELECT count(*) FROM decisions WHERE transaction_id = $1", review.Decision.TransactionId); n != 1 {
		t.Fatalf("decisions rows %d", n)
	}
	if n := count(t, pool, "SELECT count(*) FROM decision_outbox WHERE decision_id = $1 AND sent_at IS NULL AND payload->>'action' = 'REVIEW'", review.Decision.DecisionId); n != 1 {
		t.Fatalf("outbox rows %d", n)
	}
	if n := count(t, pool, "SELECT count(*) FROM review_cases WHERE decision_id = $1 AND status = 'open'", review.Decision.DecisionId); n != 1 {
		t.Fatalf("review cases %d", n)
	}
	approve := record(2, fraudv1.Action_ACTION_APPROVE)
	approve.Decision.Score = -1
	if _, err := w.Commit(ctx, approve); err != nil {
		t.Fatal(err)
	}
	if n := count(t, pool, "SELECT count(*) FROM review_cases WHERE decision_id = $1", approve.Decision.DecisionId); n != 0 {
		t.Fatal("APPROVE must not open a review case")
	}
	if n := count(t, pool, "SELECT count(*) FROM decisions WHERE transaction_id = $1 AND score IS NULL", approve.Decision.TransactionId); n != 1 {
		t.Fatal("score -1 must be stored as NULL")
	}
	// A second decision for the same transaction loses: the stored one is returned, nothing is duplicated.
	dup := record(1, fraudv1.Action_ACTION_DECLINE)
	dup.Decision.DecisionId = "0192f000-0000-7000-9000-999999999999"
	got, err = w.Commit(ctx, dup)
	if err != nil || got.DecisionId != review.Decision.DecisionId || got.Action != fraudv1.Action_ACTION_REVIEW {
		t.Fatalf("duplicate returned %+v, %v", got, err)
	}
	if n := count(t, pool, "SELECT count(*) FROM decision_outbox"); n != 2 {
		t.Fatalf("outbox rows %d, want 2", n)
	}
	stored, err := w.GetByTransaction(ctx, review.Decision.TransactionId)
	if err != nil || stored == nil || stored.DecisionId != review.Decision.DecisionId || len(stored.ReasonCodes) != 2 {
		t.Fatalf("GetByTransaction %+v %v", stored, err)
	}
	if missing, err := w.GetByTransaction(ctx, "0192f000-0000-7000-8000-000000000999"); missing != nil || err != nil {
		t.Fatalf("missing tx: %v %v", missing, err)
	}
}

func TestGroupCommitBatchesConcurrentWrites(t *testing.T) {
	pool := newPool(t, migratedDB(t))
	w, err := NewGroupWriter(pool, WriterConfig{SpoolDir: t.TempDir()}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	const n = 500
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := w.Commit(ctx, record(1000+i, fraudv1.Action_ACTION_APPROVE))
			errs <- err
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if got := count(t, pool, "SELECT count(*) FROM decisions"); got != n {
		t.Fatalf("decisions %d", got)
	}
	if w.Stats().Batches >= n {
		t.Fatalf("no batching: %d batches for %d decisions", w.Stats().Batches, n)
	}
}

func TestSpoolReplayAfterPostgresRecovers(t *testing.T) {
	dsn := migratedDB(t)
	u, _ := url.Parse(dsn)
	proxy := testutil.NewProxy(t, u.Host)
	u.Host = proxy.Addr()
	pool := newPool(t, u.String())
	spoolDir := t.TempDir()
	w, err := NewGroupWriter(pool, WriterConfig{SpoolDir: spoolDir, ReplayInterval: 50 * time.Millisecond}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	proxy.Cut()
	const n = 20
	for i := 0; i < n; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		start := time.Now()
		d, err := w.Commit(ctx, record(2000+i, fraudv1.Action_ACTION_REVIEW))
		cancel()
		if err != nil {
			t.Fatalf("commit during outage must spool, got %v", err)
		}
		if d == nil || time.Since(start) > 60*time.Millisecond {
			t.Fatalf("spooled commit took %v", time.Since(start))
		}
	}
	if w.SpoolDepth() < n {
		t.Fatalf("spool depth %d, want >= %d", w.SpoolDepth(), n)
	}
	// One record spooled twice (for example a timeout racing a late batch) must still land once.
	if err := w.spool.Append([]DecisionRecord{record(2000, fraudv1.Action_ACTION_REVIEW)}); err != nil {
		t.Fatal(err)
	}

	proxy.Restore()
	direct := newPool(t, dsn)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && (w.SpoolDepth() > 0 || count(t, direct, "SELECT count(*) FROM decisions") < n) {
		time.Sleep(20 * time.Millisecond)
	}
	if w.SpoolDepth() != 0 {
		t.Fatalf("spool not drained: %d", w.SpoolDepth())
	}
	if got := count(t, direct, "SELECT count(*) FROM decisions"); got != n {
		t.Fatalf("decisions %d, want %d (no loss, no duplicates)", got, n)
	}
	if got := count(t, direct, "SELECT count(*) FROM decision_outbox"); got != n {
		t.Fatalf("outbox %d, want %d", got, n)
	}
	if got := count(t, direct, "SELECT count(*) FROM review_cases"); got != n {
		t.Fatalf("review cases %d, want %d", got, n)
	}
	// The breaker closed with the drain: new decisions go to Postgres again, not the spool.
	if w.Degraded() {
		t.Fatal("breaker still open after recovery")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	spooled := w.Stats().Spooled
	if _, err := w.Commit(ctx, record(2999, fraudv1.Action_ACTION_APPROVE)); err != nil || w.Stats().Spooled != spooled {
		t.Fatalf("post-recovery commit: %v, spooled %d -> %d", err, spooled, w.Stats().Spooled)
	}
	if got := count(t, direct, "SELECT count(*) FROM decisions"); got != n+1 {
		t.Fatalf("decisions %d after recovery, want %d", got, n+1)
	}
}

// hungListener accepts TCP connections and never answers: Postgres hung, not refusing.
func hungListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return ln.Addr().String()
}

func TestHungPostgresSpoolsInsteadOfOverloading(t *testing.T) {
	u, _ := url.Parse(testutil.PostgresAdminDSN(t))
	u.Host = hungListener(t)
	pool := newPool(t, u.String())
	w, err := NewGroupWriter(pool, WriterConfig{SpoolDir: t.TempDir(), QueueSize: 1000, ReplayInterval: 50 * time.Millisecond}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	const workers, perWorker = 32, 100
	var overloaded, failed, ok atomic.Int64
	var wg sync.WaitGroup
	for g := range workers {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range perWorker {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
				d, err := w.Commit(ctx, record(10_000+g*perWorker+i, fraudv1.Action_ACTION_APPROVE))
				cancel()
				switch {
				case errors.Is(err, ErrOverloaded):
					overloaded.Add(1)
				case err != nil || d == nil:
					failed.Add(1)
				default:
					ok.Add(1)
				}
			}
		}(g)
	}
	wg.Wait()
	if overloaded.Load() != 0 || failed.Load() != 0 || ok.Load() != workers*perWorker {
		t.Fatalf("hung Postgres: %d overloaded, %d failed, %d ok of %d", overloaded.Load(), failed.Load(), ok.Load(), workers*perWorker)
	}
	if !w.Degraded() || w.SpoolDepth() < workers*perWorker {
		t.Fatalf("breaker open %t, spool depth %d", w.Degraded(), w.SpoolDepth())
	}
	done := make(chan struct{})
	go func() { w.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close hung behind the unresponsive Postgres")
	}
}

func TestBreakerClosesUnderContinuousTraffic(t *testing.T) {
	dsn := migratedDB(t)
	u, _ := url.Parse(dsn)
	proxy := testutil.NewProxy(t, u.Host)
	u.Host = proxy.Addr()
	pool := newPool(t, u.String())
	w, err := NewGroupWriter(pool, WriterConfig{SpoolDir: t.TempDir(), ReplayInterval: 100 * time.Millisecond}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	var next atomic.Int64
	var committed, failed atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
				_, err := w.Commit(ctx, record(int(20_000+next.Add(1)), fraudv1.Action_ACTION_APPROVE))
				cancel()
				if err != nil {
					failed.Add(1)
				} else {
					committed.Add(1)
				}
				time.Sleep(2 * time.Millisecond) // about 4,000 commits/s across the workers
			}
		}()
	}
	proxy.Cut()
	deadline := time.Now().Add(5 * time.Second)
	for !w.Degraded() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !w.Degraded() {
		close(stop)
		wg.Wait()
		t.Fatal("breaker did not open during the outage")
	}
	proxy.Restore()
	restored := time.Now()
	for w.Degraded() && time.Since(restored) < 3*time.Second {
		time.Sleep(10 * time.Millisecond)
	}
	closedAfter := time.Since(restored)
	rowsAtClose := w.Stats().Rows
	time.Sleep(300 * time.Millisecond) // traffic keeps flowing with the breaker closed
	close(stop)
	wg.Wait()
	if w.Degraded() {
		t.Fatalf("breaker still open %v after Postgres recovered under continuous traffic", closedAfter)
	}
	if w.Stats().Rows <= rowsAtClose {
		t.Fatal("commits after recovery must go to Postgres, not the spool")
	}
	// Everything committed reaches Postgres exactly once.
	direct := newPool(t, dsn)
	end := time.Now().Add(15 * time.Second)
	for time.Now().Before(end) && (w.SpoolDepth() > 0 || count(t, direct, "SELECT count(*) FROM decisions") < int(committed.Load())) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := count(t, direct, "SELECT count(*) FROM decisions"); got != int(committed.Load()) || failed.Load() != 0 {
		t.Fatalf("decisions %d, committed %d, failed %d", got, committed.Load(), failed.Load())
	}
}

func TestCommitAfterCloseFailsFast(t *testing.T) {
	w, err := NewGroupWriter(newPool(t, migratedDB(t)), WriterConfig{SpoolDir: t.TempDir()}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	w.Close() // idempotent
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	if _, err := w.Commit(ctx, record(3000, fraudv1.Action_ACTION_APPROVE)); !errors.Is(err, ErrWriterClosed) || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("commit after close: %v after %v", err, time.Since(start))
	}
}
