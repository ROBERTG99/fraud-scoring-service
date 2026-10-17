package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Retention is the data-retention policy (spec §4.5).
type Retention struct {
	Decisions         time.Duration // decisions; their outbox, case, summary, label and shadow rows cascade
	OutboxSent        time.Duration // published outbox rows (unsent rows stay until published)
	StepRecords       time.Duration
	ProcessedMessages time.Duration
	AccountEvents     time.Duration // not in the spec's list: kept as long as the decisions they explain (D-040)
}

// DefaultRetention: 400 days for decisions and everything attached, 7 days for sent outbox rows, 90 days
// for step records and processed messages. The audit log (7 years) is never deleted here: it is append-only
// by trigger and hash-chained, so pruning it needs the migration role and a re-anchored chain (D-040).
func DefaultRetention() Retention {
	const day = 24 * time.Hour
	return Retention{Decisions: 400 * day, OutboxSent: 7 * day, StepRecords: 90 * day, ProcessedMessages: 90 * day,
		AccountEvents: 400 * day}
}

// RetentionResult counts deleted rows per table (cascaded rows are not counted).
type RetentionResult struct {
	Decisions, OutboxSent, StepRecords, ProcessedMessages, AccountEvents int64
}

// retentionDeletes are batched DELETEs: $1 cutoff, $2 batch size. Each deletes at most one batch, oldest first.
var retentionDeletes = []struct {
	name string
	sql  string
	age  func(Retention) time.Duration
	out  func(*RetentionResult) *int64
}{
	{"decisions", `DELETE FROM decisions WHERE decision_id IN
	   (SELECT decision_id FROM decisions WHERE decided_at < $1 ORDER BY decided_at LIMIT $2)`,
		func(r Retention) time.Duration { return r.Decisions }, func(x *RetentionResult) *int64 { return &x.Decisions }},
	{"decision_outbox", `DELETE FROM decision_outbox WHERE id IN
	   (SELECT id FROM decision_outbox WHERE sent_at < $1 ORDER BY sent_at LIMIT $2)`,
		func(r Retention) time.Duration { return r.OutboxSent }, func(x *RetentionResult) *int64 { return &x.OutboxSent }},
	{"step_records", `DELETE FROM step_records WHERE (run_id, step) IN
	   (SELECT run_id, step FROM step_records WHERE started_at < $1 ORDER BY started_at LIMIT $2)`,
		func(r Retention) time.Duration { return r.StepRecords }, func(x *RetentionResult) *int64 { return &x.StepRecords }},
	{"processed_messages", `DELETE FROM processed_messages WHERE (consumer, message_key) IN
	   (SELECT consumer, message_key FROM processed_messages WHERE processed_at < $1 ORDER BY processed_at LIMIT $2)`,
		func(r Retention) time.Duration { return r.ProcessedMessages }, func(x *RetentionResult) *int64 { return &x.ProcessedMessages }},
	{"account_events", `DELETE FROM account_events WHERE event_id IN
	   (SELECT event_id FROM account_events WHERE event_ts < $1 ORDER BY event_ts LIMIT $2)`,
		func(r Retention) time.Duration { return r.AccountEvents }, func(x *RetentionResult) *int64 { return &x.AccountEvents }},
}

// RunRetention deletes rows older than the policy relative to now, in batches of batch rows (short
// transactions, so the scorer's writes are never blocked for long). It is idempotent.
func RunRetention(ctx context.Context, pool *pgxpool.Pool, now time.Time, p Retention, batch int) (RetentionResult, error) {
	var res RetentionResult
	for _, d := range retentionDeletes {
		cutoff := now.Add(-d.age(p))
		for {
			bctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			tag, err := pool.Exec(bctx, d.sql, cutoff, batch)
			cancel()
			if err != nil {
				return res, fmt.Errorf("retention %s: %w", d.name, err)
			}
			*d.out(&res) += tag.RowsAffected()
			if tag.RowsAffected() < int64(batch) {
				break
			}
		}
	}
	return res, nil
}
