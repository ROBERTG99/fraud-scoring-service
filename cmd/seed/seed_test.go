//go:build integration

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/api"
	"github.com/ROBERTG99/fraud-scoring-service/internal/events"
	"github.com/ROBERTG99/fraud-scoring-service/internal/features"
	"github.com/ROBERTG99/fraud-scoring-service/internal/model"
	"github.com/ROBERTG99/fraud-scoring-service/internal/replay"
	"github.com/ROBERTG99/fraud-scoring-service/internal/rules"
	"github.com/ROBERTG99/fraud-scoring-service/internal/store"
	"github.com/ROBERTG99/fraud-scoring-service/internal/testutil"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type fixedModel struct{}

func (fixedModel) Predict(_ context.Context, _ string, rows [][]float32) (model.Prediction, error) {
	out := make([]float32, len(rows))
	for i, r := range rows {
		out[i] = 0.05
		if r[features.IdxAmountMinor] >= 50_000 {
			out[i] = 0.6 // REVIEW band (0.30..0.85)
		}
	}
	return model.Prediction{Scores: out, Version: "fraud-gbdt/test"}, nil
}

type localScorer struct{ s *api.ScorerServer }

func (l localScorer) Score(ctx context.Context, in *fraudv1.ScoreRequest, _ ...grpc.CallOption) (*fraudv1.Decision, error) {
	return l.s.Score(ctx, in)
}

// writeDataset stands in for cmd/gen: 40 accounts, every fourth transaction large.
func writeDataset(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	base := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC).UnixMilli()
	var buf []byte
	for i := range 160 {
		acc := fmt.Sprintf("acc_%08d", i%40)
		ts := base + int64(i)*30_000
		amount := int64(2_000)
		if i%4 == 0 {
			amount = 90_000
		}
		e := events.Event{Kind: events.KindTransaction, EventID: fmt.Sprintf("0192f000-0000-7000-8000-%012d", i), AccountID: acc,
			TSMillis: ts, DeviceID: fmt.Sprintf("%064x", i%40), IPPrefix: "203.0.113.0/24", Country: "US", AmountMinor: amount,
			Currency: "USD", MerchantID: "m_00042", MCC: "5732", Channel: events.ChannelEcommerce}
		buf = append(e.AppendJSON(buf), '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), buf, 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"size":"test"}`+"\n"), 0o600)
}

func TestSeedIdempotent(t *testing.T) {
	ownerDSN := testutil.NewDatabase(t)
	appDSN := testutil.WithUser(t, ownerDSN, "fraud_app", "fraud_app")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := store.MigrateUp(ctx, ownerDSN); err != nil { // the roles must exist before the pool connects
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, appDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	writer, err := store.NewGroupWriter(pool, store.WriterConfig{SpoolDir: t.TempDir()}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	spec, err := features.LoadSpec("../../features/spec.yaml")
	if err != nil {
		t.Fatal(err)
	}
	eng, err := rules.NewEngine("../../rules", nil)
	if err != nil {
		t.Fatal(err)
	}
	rdb := features.NewRedisClient(testutil.RedisAddr(t))
	defer func() { _ = rdb.Close() }()
	prefix := testutil.UniqueName("seed")
	srv := api.NewScorerServer(api.ScorerDeps{Features: features.NewStore(rdb, prefix, spec), Model: fixedModel{}, Rules: eng,
		Idempotency: store.NewRedisIdempotency(rdb, prefix, []byte("seed test idempotency key, 32 b!")), Decisions: writer, Log: quiet,
		MaxEventAge: -1}) // historical synthetic data, as the seed stack runs it (D-041)

	dataDir := filepath.Join(t.TempDir(), "demo")
	var gens, trains atomic.Int32
	trained := atomic.Bool{}
	d := deps{
		Exec: func(_ context.Context, argv []string) error {
			switch argv[0] {
			case "gen":
				gens.Add(1)
				return writeDataset(dataDir)
			case "train":
				trains.Add(1)
				trained.Store(true)
				return nil
			}
			return fmt.Errorf("unexpected command %v", argv)
		},
		ModelReady: func(context.Context) bool { return trained.Load() },
		Scorer:     localScorer{srv},
	}
	cfg := config{DataDir: dataDir, MigrateDSN: ownerDSN, AppDSN: appDSN, GenCmd: []string{"gen"}, TrainCmds: [][]string{{"train"}},
		ModelWait: time.Second, Window: 24 * time.Hour, Replay: replay.Config{Rate: 0, Workers: 8}, MinCases: 40, MaxErrRate: 0, PersistWait: 30 * time.Second}

	first, err := seed(ctx, cfg, d, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Generated || !first.Trained || first.Stats.Scored != 160 || first.Decisions != 160 || first.Missing != 0 || first.OpenCases != 40 {
		t.Fatalf("first run %+v", first)
	}
	ids := decisionIDs(t, pool)

	second, err := seed(ctx, cfg, d, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if second.Generated || second.Trained || second.Decisions != first.Decisions || second.OpenCases != first.OpenCases {
		t.Fatalf("second run must change nothing: %+v", second)
	}
	if gens.Load() != 1 || trains.Load() != 1 {
		t.Fatalf("gen ran %d times, train %d times", gens.Load(), trains.Load())
	}
	again := decisionIDs(t, pool)
	if len(again) != len(ids) {
		t.Fatalf("decisions %d then %d", len(ids), len(again))
	}
	for tx, id := range ids {
		if again[tx] != id {
			t.Fatalf("transaction %s got a new decision on replay", tx)
		}
	}

	cfg.MinCases = 41
	if _, err := seed(ctx, cfg, d, quiet); err == nil {
		t.Fatal("too few open cases must fail the seed")
	}
}

func decisionIDs(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT transaction_id, decision_id::text FROM decisions`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var tx, id string
		if err := rows.Scan(&tx, &id); err != nil {
			t.Fatal(err)
		}
		out[tx] = id
	}
	return out
}
