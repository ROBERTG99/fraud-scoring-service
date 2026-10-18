// Command retention applies the data-retention policy (spec §4.5, `make retention`): it deletes decisions
// older than 400 days (their outbox, case, summary, label and shadow rows cascade), published outbox rows
// older than 7 days, step records and processed messages older than 90 days, and account events older than
// 400 days, in short batches. The audit log is never deleted (D-040). Run it daily; it is idempotent.
//
// Env: FRAUD_PG_DSN (the fraud_retention role, D-043), FRAUD_RETENTION_BATCH (rows per batch, default 5000).
package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ROBERTG99/fraud-scoring-service/internal/store"
	"github.com/ROBERTG99/fraud-scoring-service/internal/telemetry"
)

func main() {
	log := telemetry.NewLogger("retention")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	batch, err := strconv.Atoi(envOr("FRAUD_RETENTION_BATCH", "5000"))
	if err != nil || batch < 1 {
		log.Error("FRAUD_RETENTION_BATCH must be a positive integer")
		os.Exit(2)
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	pool, err := pgxpool.New(cctx, envOr("FRAUD_PG_DSN", "postgres://fraud_retention:fraud_retention@127.0.0.1:8520/fraud?sslmode=disable"))
	cancel()
	if err != nil {
		log.Error("connect", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
	start := time.Now()
	res, err := store.RunRetention(ctx, pool, start.UTC(), store.DefaultRetention(), batch)
	log.Info("retention", "decisions", res.Decisions, "outbox_sent", res.OutboxSent, "step_records", res.StepRecords,
		"processed_messages", res.ProcessedMessages, "summary_attempts", res.SummaryAttempts, "account_events", res.AccountEvents,
		"duration_ms", time.Since(start).Milliseconds())
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Error("retention failed", "err", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
