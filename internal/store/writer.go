package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/telemetry"
)

// ErrOverloaded means the commit queue stayed full for the enqueue budget (5 ms); the API maps it to
// RESOURCE_EXHAUSTED.
var ErrOverloaded = errors.New("decision writer overloaded")

// WriterConfig tunes the group-commit writer (defaults from spec §4.7).
type WriterConfig struct {
	SpoolDir       string
	QueueSize      int           // 10,000
	MaxBatch       int           // 256
	FlushEvery     time.Duration // 2 ms
	EnqueueWait    time.Duration // 5 ms
	BatchTimeout   time.Duration // deadline of one batch transaction (200 ms)
	ReplayInterval time.Duration // spool replay attempts (1 s)
}

func (c *WriterConfig) defaults() {
	if c.QueueSize == 0 {
		c.QueueSize = 10_000
	}
	if c.MaxBatch == 0 {
		c.MaxBatch = 256
	}
	if c.FlushEvery == 0 {
		c.FlushEvery = 2 * time.Millisecond
	}
	if c.EnqueueWait == 0 {
		c.EnqueueWait = 5 * time.Millisecond
	}
	if c.BatchTimeout == 0 {
		c.BatchTimeout = 200 * time.Millisecond
	}
	if c.ReplayInterval == 0 {
		c.ReplayInterval = time.Second
	}
}

type commitResult struct {
	d   *fraudv1.Decision
	err error
}

type pending struct {
	rec  DecisionRecord
	done chan commitResult // buffered(1): the flusher never blocks on a gone caller
	// abandoned is set by a caller that gave up and spooled; the flusher skips it.
	abandoned atomic.Bool
}

// WriterStats are counters for tests and metrics.
type WriterStats struct {
	Batches  int64
	Rows     int64
	Spooled  int64
	Replayed int64
}

// GroupWriter persists decisions in group commits: one transaction per batch writes decisions,
// decision_outbox and (for REVIEW) review_cases before any caller in the batch gets its response.
// When Postgres fails or a caller's deadline (30 ms) passes, decisions go to the disk spool instead
// and are replayed later; every insert is idempotent on transaction_id. A failed batch opens a breaker
// so that a hung (not refusing) Postgres never backs the queue up into ErrOverloaded.
type GroupWriter struct {
	pool  *pgxpool.Pool
	cfg   WriterConfig
	log   *slog.Logger
	queue chan *pending
	spool *Spool
	stop  chan struct{}
	wg    sync.WaitGroup
	stats struct{ batches, rows, spooled, replayed atomic.Int64 }
	// down is the breaker: after a failed batch every Commit spools directly (no queueing behind a hung
	// Postgres) until the replay loop reaches Postgres and drains the spool.
	down   atomic.Bool
	closed atomic.Bool
}

// ErrWriterClosed is returned by Commit after Close.
var ErrWriterClosed = errors.New("decision writer closed")

// NewGroupWriter starts the flusher and the spool replayer.
func NewGroupWriter(pool *pgxpool.Pool, cfg WriterConfig, log *slog.Logger) (*GroupWriter, error) {
	cfg.defaults()
	if cfg.SpoolDir == "" {
		return nil, errors.New("writer: SpoolDir is required")
	}
	sp, err := OpenSpool(cfg.SpoolDir)
	if err != nil {
		return nil, err
	}
	w := &GroupWriter{pool: pool, cfg: cfg, log: log, queue: make(chan *pending, cfg.QueueSize), spool: sp, stop: make(chan struct{})}
	telemetry.SpoolDepth.Set(float64(sp.Depth()))
	w.wg.Add(2)
	go w.flushLoop()
	go w.replayLoop()
	return w, nil
}

// Close stops background work after flushing queued decisions.
func (w *GroupWriter) Close() {
	if w.closed.Swap(true) {
		return
	}
	close(w.stop)
	w.wg.Wait()
	_ = w.spool.Close()
}

// Stats returns counters.
func (w *GroupWriter) Stats() WriterStats {
	return WriterStats{Batches: w.stats.batches.Load(), Rows: w.stats.rows.Load(), Spooled: w.stats.spooled.Load(), Replayed: w.stats.replayed.Load()}
}

// SpoolDepth is the number of decisions waiting in the spool.
func (w *GroupWriter) SpoolDepth() int { return w.spool.Depth() }

// Commit enqueues rec and waits until its batch commits. It returns the stored decision: rec's own,
// or the earlier decision for the same transaction_id. On Postgres failure or when ctx expires the
// decision is spooled and returned (the caller responds; replay persists it later).
func (w *GroupWriter) Commit(ctx context.Context, rec DecisionRecord) (*fraudv1.Decision, error) {
	if w.closed.Load() {
		return nil, ErrWriterClosed
	}
	if w.down.Load() {
		return w.spoolOne(rec)
	}
	p := &pending{rec: rec, done: make(chan commitResult, 1)}
	// Enqueue without a timer first: if the goroutine was descheduled past EnqueueWait, a select over a
	// free queue slot and an expired timer would pick at random and report a spurious overload.
	select {
	case w.queue <- p:
	default:
		enqueue := time.NewTimer(w.cfg.EnqueueWait)
		defer enqueue.Stop()
		select {
		case w.queue <- p:
		case <-enqueue.C:
			return nil, ErrOverloaded
		case <-ctx.Done():
			return w.spoolOne(rec)
		}
	}
	select {
	case r := <-p.done:
		return r.d, r.err
	case <-ctx.Done():
		// Too slow: spool now and tell the flusher to skip the entry. If its batch is already being
		// written, the row may land twice in effect (Postgres and spool); replay's insert is a no-op.
		p.abandoned.Store(true)
		return w.spoolOne(rec)
	}
}

func (w *GroupWriter) spoolOne(rec DecisionRecord) (*fraudv1.Decision, error) {
	if err := w.spool.Append([]DecisionRecord{rec}); err != nil {
		return nil, fmt.Errorf("spool: %w", err)
	}
	w.stats.spooled.Add(1)
	telemetry.SpoolDepth.Set(float64(w.spool.Depth()))
	return rec.Decision, nil
}

func (w *GroupWriter) flushLoop() {
	defer w.wg.Done()
	batch := make([]*pending, 0, w.cfg.MaxBatch)
	tick := time.NewTicker(w.cfg.FlushEvery)
	defer tick.Stop()
	flush := func() {
		if len(batch) == 0 {
			return
		}
		w.flush(batch)
		batch = batch[:0]
	}
	for {
		select {
		case p := <-w.queue:
			batch = append(batch, p)
			if len(batch) >= w.cfg.MaxBatch {
				flush()
			}
		case <-tick.C:
			flush()
		case <-w.stop:
			for {
				select {
				case p := <-w.queue:
					batch = append(batch, p)
				default:
					flush()
					return
				}
			}
		}
	}
}

func (w *GroupWriter) flush(all []*pending) {
	batch := make([]*pending, 0, len(all))
	for _, p := range all {
		if !p.abandoned.Load() {
			batch = append(batch, p)
		}
	}
	if len(batch) == 0 {
		return
	}
	recs := make([]DecisionRecord, len(batch))
	for i, p := range batch {
		recs[i] = p.rec
	}
	var stored []*fraudv1.Decision
	err := errBreakerOpen
	if !w.down.Load() {
		ctx, cancel := context.WithTimeout(context.Background(), w.cfg.BatchTimeout)
		stored, err = w.insertBatch(ctx, recs)
		cancel()
		w.stats.batches.Add(1)
	}
	if err != nil {
		if !errors.Is(err, errBreakerOpen) && !w.down.Swap(true) {
			telemetry.WriterDegraded.Set(1)
			w.log.Warn("group commit failed; spooling until Postgres recovers", "rows", len(recs), "err", err)
		}
		if serr := w.spool.Append(recs); serr != nil {
			for _, p := range batch {
				p.done <- commitResult{err: fmt.Errorf("postgres: %v; spool: %w", err, serr)}
			}
			return
		}
		w.stats.spooled.Add(int64(len(recs)))
		telemetry.SpoolDepth.Set(float64(w.spool.Depth()))
		for _, p := range batch {
			p.done <- commitResult{d: p.rec.Decision}
		}
		return
	}
	w.stats.rows.Add(int64(len(recs)))
	for i, p := range batch {
		p.done <- commitResult{d: stored[i]}
	}
}

const insertDecisionSQL = `
WITH ins AS (
  INSERT INTO decisions (decision_id, transaction_id, account_id, action, score, reason_codes, model_version, rules_version,
                         degraded, features, latency_ms, decided_at, tx)
  VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $16)
  ON CONFLICT DO NOTHING
  RETURNING decision_id
), ob AS (
  INSERT INTO decision_outbox (decision_id, payload, created_at)
  SELECT decision_id, $13, $12 FROM ins
), rc AS (
  INSERT INTO review_cases (case_id, decision_id, account_id, status, priority, created_at)
  SELECT $14, decision_id, $3, 'open', $15, $12 FROM ins WHERE $4 = 'REVIEW'
)
SELECT count(*) FROM ins`

// insertBatch writes all records in one transaction and returns the stored decision per record.
func (w *GroupWriter) insertBatch(ctx context.Context, recs []DecisionRecord) ([]*fraudv1.Decision, error) {
	tx, err := w.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	b := &pgx.Batch{}
	for i := range recs {
		r := &recs[i]
		d := r.Decision
		var score any
		if d.Score >= 0 {
			score = d.Score
		}
		feats, err := json.Marshal(r.Features)
		if err != nil {
			return nil, err
		}
		codes := d.ReasonCodes
		if codes == nil {
			codes = []string{}
		}
		attrs, err := json.Marshal(r.Tx)
		if err != nil {
			return nil, err
		}
		b.Queue(insertDecisionSQL, d.DecisionId, d.TransactionId, r.AccountID, ActionName(d.Action), score, codes, d.ModelVersion,
			d.RulesVersion, d.Degraded, string(feats), d.LatencyMs, d.DecidedAt.AsTime(), string(r.OutboxPayload()), uuid.NewString(), r.Priority(),
			string(attrs))
	}
	br := tx.SendBatch(ctx, b)
	inserted := make([]bool, len(recs))
	for i := range recs {
		var n int
		if err := br.QueryRow().Scan(&n); err != nil {
			_ = br.Close()
			return nil, err
		}
		inserted[i] = n == 1
	}
	if err := br.Close(); err != nil {
		return nil, err
	}
	out := make([]*fraudv1.Decision, len(recs))
	var conflicts []string
	for i := range recs {
		if inserted[i] {
			out[i] = recs[i].Decision
		} else {
			conflicts = append(conflicts, recs[i].Decision.TransactionId)
		}
	}
	if len(conflicts) > 0 {
		existing, err := fetchDecisions(ctx, tx, conflicts)
		if err != nil {
			return nil, err
		}
		for i := range recs {
			if out[i] == nil {
				out[i] = existing[recs[i].Decision.TransactionId]
				if out[i] == nil {
					out[i] = recs[i].Decision // conflicted on decision_id only (replay of the same decision)
				}
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func fetchDecisions(ctx context.Context, q querier, txIDs []string) (map[string]*fraudv1.Decision, error) {
	rows, err := q.Query(ctx, `SELECT decision_id::text, transaction_id, action, score, reason_codes, model_version, rules_version, degraded,
		latency_ms, decided_at FROM decisions WHERE transaction_id = ANY($1)`, txIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*fraudv1.Decision{}
	for rows.Next() {
		var (
			d         fraudv1.Decision
			action    string
			score     *float32
			decidedAt time.Time
		)
		if err := rows.Scan(&d.DecisionId, &d.TransactionId, &action, &score, &d.ReasonCodes, &d.ModelVersion, &d.RulesVersion,
			&d.Degraded, &d.LatencyMs, &decidedAt); err != nil {
			return nil, err
		}
		d.Action = fraudv1.Action(fraudv1.Action_value["ACTION_"+action])
		d.Score = -1
		if score != nil {
			d.Score = *score
		}
		d.DecidedAt = timestamppb.New(decidedAt)
		out[d.TransactionId] = &d
	}
	return out, rows.Err()
}

// GetByTransaction returns the stored decision for a transaction, or (nil, nil).
func (w *GroupWriter) GetByTransaction(ctx context.Context, txID string) (*fraudv1.Decision, error) {
	m, err := fetchDecisions(ctx, w.pool, []string{txID})
	if err != nil {
		return nil, err
	}
	return m[txID], nil
}

// errBreakerOpen marks batches spooled without trying Postgres.
var errBreakerOpen = errors.New("postgres breaker open")

func (w *GroupWriter) replayLoop() {
	defer w.wg.Done()
	t := time.NewTicker(w.cfg.ReplayInterval)
	defer t.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-t.C:
			if w.down.Load() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				err := w.pool.Ping(ctx)
				cancel()
				if err != nil {
					continue
				}
				// Postgres answers: resume group commit now and let this loop drain the backlog in the
				// background. Waiting for an empty spool would never happen under traffic, because every
				// Commit spools while the breaker is open. Inserts are idempotent on transaction_id, so new
				// commits and replayed rows can overlap safely; a failing batch simply opens it again.
				w.closeBreaker()
			}
			if w.spool.Depth() == 0 {
				continue
			}
			n, err := w.spool.Drain(func(recs []DecisionRecord) error {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_, err := w.insertBatch(ctx, recs)
				return err
			})
			w.stats.replayed.Add(int64(n))
			telemetry.SpoolDepth.Set(float64(w.spool.Depth()))
			if err != nil {
				w.log.Debug("spool replay deferred", "err", err)
				continue
			}
			if n > 0 {
				w.log.Info("spool replayed", "decisions", n)
			}
		}
	}
}

func (w *GroupWriter) closeBreaker() {
	if w.down.Swap(false) {
		telemetry.WriterDegraded.Set(0)
		w.log.Info("postgres reachable again; group commit resumed")
	}
}

// Degraded reports whether the breaker is open (decisions go straight to the spool).
func (w *GroupWriter) Degraded() bool { return w.down.Load() }
