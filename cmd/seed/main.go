// Command seed prepares a demo environment (compose `seed` job, `make seed`): migrate, generate the
// demo dataset, train and register the model, replay the last 24 h through the scorer, and check that
// enough review cases exist. Every step is idempotent, so seed can run again on a seeded stack.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/replay"
	"github.com/ROBERTG99/fraud-scoring-service/internal/store"
	"github.com/ROBERTG99/fraud-scoring-service/internal/stream"
	"github.com/ROBERTG99/fraud-scoring-service/internal/telemetry"
)

type config struct {
	DataDir     string
	MigrateDSN  string
	AppDSN      string
	GenCmd      []string   // run when DataDir has no manifest.json
	TrainCmds   [][]string // run when the model server is not ready
	ModelWait   time.Duration
	Window      time.Duration
	Replay      replay.Config
	MinCases    int
	MaxErrRate  float64
	PersistWait time.Duration // how long to wait for every scored decision to reach Postgres
}

// deps are the side effects, injected in tests.
type deps struct {
	Exec       func(ctx context.Context, argv []string) error
	ModelReady func(ctx context.Context) bool
	Producer   replay.Producer // nil: no Kafka (tests)
	Scorer     replay.Scorer
}

type report struct {
	Generated  bool
	Trained    bool
	Stats      replay.Stats
	OpenCases  int
	Decisions  int
	Missing    int // scored transactions without a decision row when the wait ended
	ModelReady bool
}

func seed(ctx context.Context, cfg config, d deps, log *slog.Logger) (report, error) {
	var r report
	mctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	err := store.MigrateUp(mctx, cfg.MigrateDSN)
	cancel()
	if err != nil {
		return r, fmt.Errorf("migrate: %w", err)
	}
	log.Info("seed: migrations applied")

	if _, err := os.Stat(filepath.Join(cfg.DataDir, "manifest.json")); errors.Is(err, os.ErrNotExist) {
		if err := d.Exec(ctx, cfg.GenCmd); err != nil {
			return r, fmt.Errorf("generate: %w", err)
		}
		r.Generated = true
	} else if err != nil {
		return r, err
	}

	if len(cfg.TrainCmds) > 0 {
		r.ModelReady = d.ModelReady(ctx)
		if !r.ModelReady {
			for _, c := range cfg.TrainCmds {
				if err := d.Exec(ctx, c); err != nil {
					return r, fmt.Errorf("train: %w", err)
				}
			}
			r.Trained = true
			// The model server polls MLflow for the alias; scoring before it loads would be rules-only.
			deadline := time.Now().Add(cfg.ModelWait)
			for !r.ModelReady && time.Now().Before(deadline) {
				if err := sleep(ctx, 2*time.Second); err != nil {
					return r, err
				}
				r.ModelReady = d.ModelReady(ctx)
			}
			if !r.ModelReady {
				log.Warn("seed: model server not ready; decisions will be rules-only (MODEL_UNAVAILABLE)")
			}
		}
	}

	evs, err := replay.Load(filepath.Join(cfg.DataDir, "events.jsonl"))
	if err != nil {
		return r, err
	}
	warm, live := replay.Split(evs, cfg.Window)
	r.Stats, err = replay.Run(ctx, cfg.Replay, d.Producer, d.Scorer, warm, live)
	if err != nil {
		return r, fmt.Errorf("replay: %w", err)
	}
	if total := r.Stats.Scored + r.Stats.Errors; total > 0 && float64(r.Stats.Errors)/float64(total) > cfg.MaxErrRate {
		return r, fmt.Errorf("replay: %d of %d Score calls failed (%v)", r.Stats.Errors, total, r.Stats.ByCode)
	}

	r.OpenCases, r.Decisions, r.Missing, err = countCases(ctx, cfg.AppDSN, r.Stats.TransactionIDs, cfg.PersistWait)
	if err != nil {
		return r, err
	}
	if r.Missing > 0 {
		log.Warn("seed: decisions not yet persisted (still in the scorer spool)", "missing", r.Missing)
	}
	if r.OpenCases < cfg.MinCases {
		return r, fmt.Errorf("only %d open review cases after replay, want at least %d", r.OpenCases, cfg.MinCases)
	}
	return r, nil
}

// countCases waits until every scored transaction has a decision row (decisions the scorer spooled while
// Postgres was slow are replayed by its writer), then counts. missing > 0 means the wait timed out.
func countCases(ctx context.Context, dsn string, scored []string, wait time.Duration) (open, decisions, missing int, err error) {
	cctx, cancel := context.WithTimeout(ctx, wait+10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(cctx, dsn)
	if err != nil {
		return 0, 0, 0, err
	}
	defer pool.Close()
	deadline := time.Now().Add(wait)
	for {
		var persisted int
		if err := pool.QueryRow(cctx, `SELECT count(*) FROM decisions WHERE transaction_id = ANY($1)`, scored).Scan(&persisted); err != nil {
			return 0, 0, 0, err
		}
		missing = len(scored) - persisted
		if missing == 0 || time.Now().After(deadline) {
			break
		}
		if err := sleep(cctx, 250*time.Millisecond); err != nil {
			break
		}
	}
	err = pool.QueryRow(cctx, `SELECT (SELECT count(*) FROM review_cases WHERE status = 'open'), (SELECT count(*) FROM decisions)`).
		Scan(&open, &decisions)
	return open, decisions, missing, err
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func execCmd(log *slog.Logger) func(ctx context.Context, argv []string) error {
	return func(ctx context.Context, argv []string) error {
		if len(argv) == 0 {
			return errors.New("empty command")
		}
		log.Info("seed: running", "cmd", strings.Join(argv, " "))
		c := exec.CommandContext(ctx, argv[0], argv[1:]...) // #nosec G204 -- operator-configured commands
		c.Stdout, c.Stderr = os.Stderr, os.Stderr
		return c.Run()
	}
}

func modelReady(url string) func(ctx context.Context) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	return func(ctx context.Context) bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return false
		}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

type cmdList [][]string

func (c *cmdList) String() string { return fmt.Sprint(*c) }
func (c *cmdList) Set(v string) error {
	*c = append(*c, strings.Fields(v))
	return nil
}

func main() {
	log := telemetry.NewLogger("seed")
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	data := fs.String("data", envOr("FRAUD_SEED_DATA", "artifacts/data/demo"), "dataset directory")
	size := fs.String("size", "demo", "dataset size for generation")
	seedN := fs.Uint64("seed", 7, "generator seed")
	gen := fs.String("gen-cmd", envOr("FRAUD_SEED_GEN_CMD", "gen"), "generator command (the dataset flags are appended)")
	var train cmdList
	fs.Var(&train, "train-cmd", "command run when the model server is not ready (repeatable, in order)")
	modelURL := fs.String("model-ready-url", envOr("FRAUD_MODEL_URL", "http://127.0.0.1:8514")+"/readyz", "model server readiness URL")
	modelWait := fs.Duration("model-wait", 2*time.Minute, "how long to wait for the model server after training")
	window := fs.Duration("window", 72*time.Hour, "replay window: the held-out test split (docs/DECISIONS.md D-024)")
	rate := fs.Float64("rate", 250, "transactions scored per second")
	scorer := fs.String("scorer", envOr("FRAUD_SCORER_GRPC", "127.0.0.1:8510"), "scorer gRPC address")
	brokers := fs.String("brokers", envOr("FRAUD_KAFKA_BROKERS", "127.0.0.1:8540"), "Kafka brokers")
	minCases := fs.Int("min-cases", 50, "fail when fewer open review cases exist after replay")
	_ = fs.Parse(os.Args[1:])

	token := os.Getenv("FRAUD_SERVICE_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "seed: FRAUD_SERVICE_TOKEN is required")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	kcl, err := kgo.NewClient(kgo.SeedBrokers(stream.Brokers(*brokers)...), kgo.ProducerLinger(5*time.Millisecond),
		kgo.ProducerBatchMaxBytes(1<<20), kgo.RecordPartitioner(kgo.StickyKeyPartitioner(nil)))
	if err != nil {
		log.Error("kafka", "err", err)
		os.Exit(1)
	}
	defer kcl.Close()
	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	if err := stream.EnsureTopics(tctx, kcl, stream.Partitions, 1, stream.AllTopics...); err != nil {
		cancel()
		log.Error("kafka topics", "err", err)
		os.Exit(1)
	}
	cancel()
	conn, err := grpc.NewClient(*scorer, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Error("scorer", "err", err)
		os.Exit(1)
	}
	defer func() { _ = conn.Close() }()

	wait := replay.CaughtUp(kadm.NewClient(kcl), "feature-updater", []string{stream.TopicLogin, stream.TopicDevice, stream.TopicTransaction}, 250*time.Millisecond)
	cfg := config{
		DataDir:    *data,
		MigrateDSN: envOr("FRAUD_PG_MIGRATE_DSN", "postgres://fraud:fraud@127.0.0.1:8520/fraud?sslmode=disable"),
		AppDSN:     envOr("FRAUD_PG_DSN", "postgres://fraud_app:fraud_app@127.0.0.1:8520/fraud?sslmode=disable"),
		GenCmd:     append(strings.Fields(*gen), "-size", *size, "-seed", strconv.FormatUint(*seedN, 10), "-out", *data),
		TrainCmds:  train,
		ModelWait:  *modelWait,
		Window:     *window,
		Replay: replay.Config{Rate: *rate, Workers: 32, Token: token, Log: log, WaitCaughtUp: func(ctx context.Context) error {
			wctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			defer cancel()
			return wait(wctx)
		}},
		MinCases:    *minCases,
		MaxErrRate:  0.01,
		PersistWait: time.Minute,
	}
	start := time.Now()
	r, err := seed(ctx, cfg, deps{Exec: execCmd(log), ModelReady: modelReady(*modelURL), Producer: kcl, Scorer: fraudv1.NewScorerClient(conn)}, log)
	if err != nil {
		log.Error("seed failed", "err", err)
		os.Exit(1)
	}
	fmt.Printf("seed: done in %s | generated=%t trained=%t model_ready=%t | produced %d events, scored %d (REVIEW %d, DECLINE %d, degraded %d, errors %d) | %d decisions, %d open cases\n",
		time.Since(start).Round(time.Second), r.Generated, r.Trained, r.ModelReady, r.Stats.Produced, r.Stats.Scored,
		r.Stats.Actions["REVIEW"], r.Stats.Actions["DECLINE"], r.Stats.Degraded, r.Stats.Errors, r.Decisions, r.OpenCases)
}
