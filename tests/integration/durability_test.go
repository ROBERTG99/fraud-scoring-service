//go:build integration

package integration

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/store"
	"github.com/ROBERTG99/fraud-scoring-service/internal/testutil"
)

const durabilityToken = "durability-test-token"

// scorerProc is the real scorer binary on loopback ports, with its spool in a directory that outlives it.
type scorerProc struct {
	bin, grpcAddr, httpAddr string
	env                     []string
	cmd                     *exec.Cmd
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

func (s *scorerProc) start(t *testing.T) {
	t.Helper()
	s.cmd = exec.Command(s.bin) // #nosec G204 -- the binary this test just built
	s.cmd.Env = s.env
	s.cmd.Stdout, s.cmd.Stderr = os.Stderr, os.Stderr
	if err := s.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+s.httpAddr+"/healthz", nil)
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond) // polling a deadline, not a fixed wait
	}
	t.Fatal("scorer did not become healthy within 30 s")
}

// kill sends SIGKILL: no shutdown hook, no flush; only what was committed or spooled before a response survives.
func (s *scorerProc) kill(t *testing.T) {
	t.Helper()
	if err := s.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = s.cmd.Wait()
}

func durabilityTx(i int) *fraudv1.Transaction {
	return &fraudv1.Transaction{
		TransactionId: uuid.Must(uuid.NewV7()).String(),
		AccountId:     fmt.Sprintf("acc_%08d", i%500),
		AmountMinor:   int64(1000 + i%50_000),
		Currency:      "USD",
		MerchantId:    fmt.Sprintf("m_%05d", i%200),
		Mcc:           "5411",
		Channel:       fraudv1.Channel_CHANNEL_ECOMMERCE,
		DeviceId:      fmt.Sprintf("%064x", i%900),
		IpPrefix:      "203.0.113.0/24",
		Country:       "US",
	}
}

// TestDurabilityKill: the scorer serves about 1,000 Score calls per second; halfway through,
// Postgres is cut (decisions answered from then on are spooled) and then the scorer is killed with SIGKILL.
// After Postgres is back and the scorer restarts on the same spool directory, every decision a caller received is
// in Postgres with the decision_id the caller saw.
func TestDurabilityKill(t *testing.T) {
	testDurabilityKill(t, false)
}

func testDurabilityKill(t *testing.T, loseSpool bool) (lost int) {
	root := testutil.RepoRoot()
	bin := filepath.Join(t.TempDir(), "scorer")
	build := exec.Command("go", "build", "-o", bin, "./cmd/scorer")
	build.Dir, build.Stdout, build.Stderr = root, os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build scorer: %v", err)
	}

	admin := testutil.NewDatabase(t)
	mctx, mcancel := context.WithTimeout(context.Background(), time.Minute)
	defer mcancel()
	if err := store.MigrateUp(mctx, admin); err != nil {
		t.Fatal(err)
	}
	brokers := os.Getenv("FRAUD_TEST_KAFKA_BROKERS")
	if brokers == "" {
		brokers = freeAddr(t) // nothing listens: outbox rows stay unsent, which this test does not need
	}
	// The scorer reaches Postgres through a proxy the test can cut.
	u, err := url.Parse(testutil.WithUser(t, admin, "fraud_app", "fraud_app"))
	if err != nil {
		t.Fatal(err)
	}
	pg := testutil.NewProxy(t, u.Host)
	u.Host = pg.Addr()
	spool := t.TempDir()
	s := &scorerProc{bin: bin, grpcAddr: freeAddr(t), httpAddr: freeAddr(t)}
	s.env = append(os.Environ(),
		"FRAUD_ENV=dev",
		"FRAUD_SERVICE_TOKEN="+durabilityToken,
		"FRAUD_GRPC_ADDR="+s.grpcAddr,
		"FRAUD_HTTP_ADDR="+s.httpAddr,
		"FRAUD_ADMIN_ADDR="+freeAddr(t),
		"FRAUD_PG_DSN="+u.String(),
		"FRAUD_REDIS_ADDR="+testutil.RedisAddr(t),
		"FRAUD_REDIS_PREFIX="+testutil.UniqueName("dur"),
		"FRAUD_KAFKA_BROKERS="+brokers,
		"FRAUD_MODEL_URL=http://"+freeAddr(t), // no model server: rules-only decisions, still persisted
		"FRAUD_JWKS_URL=http://"+freeAddr(t)+"/.well-known/jwks.json",
		"FRAUD_SPOOL_DIR="+spool,
		"FRAUD_RULES_DIR="+filepath.Join(root, "rules"),
		"FRAUD_FEATURE_SPEC="+filepath.Join(root, "features", "spec.yaml"),
		"FRAUD_SHADOW_ENABLED=false",
		"OTEL_EXPORTER_OTLP_ENDPOINT=",
	)
	s.start(t)

	conn, err := grpc.NewClient(s.grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	client := fraudv1.NewScorerClient(conn)

	// Load: 1,000 calls per second from 64 workers; Postgres is cut after 1 s and the scorer killed after 2 s.
	var (
		mu       sync.Mutex
		acked    = map[string]string{} // transaction_id -> decision_id the caller received
		attempts atomic.Int64
		failures atomic.Int64
		codes    = map[string]int{} // failures by gRPC code, before and after the kill
		killed   atomic.Bool
	)
	stop := make(chan struct{})
	ticks := make(chan int, 1024)
	var wg sync.WaitGroup
	for w := 0; w < 64; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range ticks {
				tx := durabilityTx(i)
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+durabilityToken)
				d, err := client.Score(ctx, &fraudv1.ScoreRequest{Transaction: tx})
				cancel()
				attempts.Add(1)
				if err != nil {
					failures.Add(1)
					phase := "before kill"
					if killed.Load() {
						phase = "after kill"
					}
					mu.Lock()
					codes[phase+": "+status.Code(err).String()]++
					mu.Unlock()
					continue
				}
				mu.Lock()
				acked[tx.TransactionId] = d.DecisionId
				mu.Unlock()
			}
		}()
	}
	go func() {
		defer close(ticks)
		tk := time.NewTicker(time.Millisecond)
		defer tk.Stop()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-tk.C:
				ticks <- i
			}
		}
	}()
	time.Sleep(time.Second) // the load phase itself, not a synchronisation wait
	pg.Cut()
	time.Sleep(time.Second)
	killed.Store(true)
	s.kill(t)
	time.Sleep(200 * time.Millisecond) // calls in flight at the kill fail; keep loading a little past it
	close(stop)
	wg.Wait()

	mu.Lock()
	n := len(acked)
	mu.Unlock()
	t.Logf("before the kill: %d attempts, %d decisions received, %d failed %v", attempts.Load(), n, failures.Load(), codes)
	// About 1,000 calls per second were offered; under -race with every package running at once fewer
	// complete, so the guard asks for the offered load and a few hundred decisions, not 1,000 received.
	if attempts.Load() < 1500 || n < 200 {
		t.Fatalf("%d calls offered and %d decisions received before the kill; the load did not reach the scorer", attempts.Load(), n)
	}

	// Postgres is back; restart on the same spool, whose decisions are replayed into Postgres.
	pg.Restore()
	if loseSpool { // the control: a scorer that loses its spool directory does lose decisions
		for i, e := range s.env {
			if len(e) > 16 && e[:16] == "FRAUD_SPOOL_DIR=" {
				s.env[i] = "FRAUD_SPOOL_DIR=" + t.TempDir()
			}
		}
	}
	s.start(t)
	defer s.kill(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(context.Background()) }()
	ids := make([]string, 0, n)
	for id := range acked {
		ids = append(ids, id)
	}
	var mismatched int
	deadline := time.Now().Add(15 * time.Second)
	for {
		lost, mismatched = 0, 0
		rows, err := db.Query(ctx, `SELECT transaction_id, decision_id::text FROM decisions WHERE transaction_id = ANY($1)`, ids)
		if err != nil {
			t.Fatal(err)
		}
		stored := map[string]string{}
		for rows.Next() {
			var tx, dec string
			if err := rows.Scan(&tx, &dec); err != nil {
				t.Fatal(err)
			}
			stored[tx] = dec
		}
		rows.Close()
		for tx, dec := range acked {
			got, ok := stored[tx]
			switch {
			case !ok:
				lost++
			case got != dec:
				mismatched++
			}
		}
		if lost == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond) // polling the spool replay against the deadline above
	}
	t.Logf("after restart: %d of %d received decisions stored, %d lost, %d with another decision_id", n-lost, n, lost, mismatched)
	if mismatched != 0 || (!loseSpool && lost != 0) {
		t.Fatalf("%d decisions lost and %d changed after SIGKILL (want 0 and 0)", lost, mismatched)
	}
	return lost
}

// The control for TestDurabilityKill: the same run with the spool directory gone after the kill loses the
// decisions answered while Postgres was cut, so the test above does exercise the spool.
func TestDurabilityKillWithoutTheSpoolLosesDecisions(t *testing.T) {
	if lost := testDurabilityKill(t, true); lost == 0 {
		t.Fatal("no decision was lost without the spool: the outage did not force any decision into it")
	}
}
