package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/ROBERTG99/fraud-scoring-service/internal/telemetry"
)

// TopicDecisions is where the relay publishes fraud.decision.v1 messages.
const TopicDecisions = "decisions"

// Producer is the subset of *kgo.Client the relay needs.
type Producer interface {
	ProduceSync(ctx context.Context, rs ...*kgo.Record) kgo.ProduceResults
}

// Relay publishes unsent decision_outbox rows to Kafka at least once (spec FR-10). Rows are locked with
// FOR UPDATE SKIP LOCKED, produced, then marked sent in the same transaction; a crash between producing
// and committing re-sends them, and consumers deduplicate by decision_id (processed_messages).
type Relay struct {
	pool      *pgxpool.Pool
	producer  Producer
	log       *slog.Logger
	batchSize int
	// afterProduce is a test hook that runs between producing and marking rows sent.
	afterProduce func() error
}

// NewRelay returns a relay that publishes up to batchSize rows per transaction.
func NewRelay(pool *pgxpool.Pool, producer Producer, log *slog.Logger, batchSize int) *Relay {
	if batchSize <= 0 {
		batchSize = 500
	}
	return &Relay{pool: pool, producer: producer, log: log, batchSize: batchSize}
}

// RunOnce publishes one batch and returns how many rows were marked sent.
func (r *Relay) RunOnce(ctx context.Context) (int, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	rows, err := tx.Query(ctx, `SELECT id, payload, coalesce(traceparent, '') FROM decision_outbox WHERE sent_at IS NULL ORDER BY id LIMIT $1 FOR UPDATE SKIP LOCKED`, r.batchSize)
	if err != nil {
		return 0, err
	}
	var ids []int64
	var recs []*kgo.Record
	for rows.Next() {
		var id int64
		var payload []byte
		var traceparent string
		if err := rows.Scan(&id, &payload, &traceparent); err != nil {
			rows.Close()
			return 0, err
		}
		var msg struct {
			AccountID string `json:"account_id"`
		}
		if err := json.Unmarshal(payload, &msg); err != nil {
			rows.Close()
			return 0, fmt.Errorf("outbox row %d: %w", id, err)
		}
		ids = append(ids, id)
		rec := &kgo.Record{Topic: TopicDecisions, Key: []byte(msg.AccountID), Value: payload}
		if traceparent != "" {
			// W3C trace context over Kafka: the case summarizer continues the Score trace (spec §14).
			rec.Headers = []kgo.RecordHeader{{Key: "traceparent", Value: []byte(traceparent)}}
		}
		recs = append(recs, rec)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, tx.Commit(ctx)
	}
	if err := r.producer.ProduceSync(ctx, recs...).FirstErr(); err != nil {
		_, _ = tx.Exec(ctx, `UPDATE decision_outbox SET attempts = attempts + 1 WHERE id = ANY($1)`, ids)
		_ = tx.Commit(ctx)
		return 0, fmt.Errorf("produce decisions: %w", err)
	}
	if r.afterProduce != nil {
		if err := r.afterProduce(); err != nil {
			return 0, err // simulated crash: transaction rolls back, rows stay unsent
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE decision_outbox SET sent_at = now(), attempts = attempts + 1 WHERE id = ANY($1)`, ids); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// Run publishes continuously until ctx ends, and keeps the outbox gauges current.
func (r *Relay) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		for {
			rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			n, err := r.RunOnce(rctx)
			cancel()
			if err != nil && !errors.Is(err, context.Canceled) {
				r.log.Warn("outbox relay", "err", err)
			}
			if err != nil || n < r.batchSize {
				break
			}
		}
		r.updateGauges(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (r *Relay) updateGauges(ctx context.Context) {
	gctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var unsent int64
	var age float64
	err := r.pool.QueryRow(gctx, `SELECT count(*), COALESCE(EXTRACT(EPOCH FROM now() - min(created_at)), 0)
		FROM decision_outbox WHERE sent_at IS NULL`).Scan(&unsent, &age)
	if err != nil {
		return
	}
	telemetry.OutboxUnsent.Set(float64(unsent))
	telemetry.OutboxOldestUnsentAge.Set(age)
}
