package store

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ROBERTG99/fraud-scoring-service/internal/events"
	"github.com/ROBERTG99/fraud-scoring-service/internal/telemetry"
)

// AccountEventWriter buffers events and inserts them into account_events in batches.
// It never blocks the caller: when the queue is full, rows are dropped and counted.
type AccountEventWriter struct {
	pool  *pgxpool.Pool
	queue chan events.Event
	log   *slog.Logger
	wg    sync.WaitGroup
}

// NewAccountEventWriter starts the background writer; Close flushes and stops it.
func NewAccountEventWriter(pool *pgxpool.Pool, log *slog.Logger) *AccountEventWriter {
	w := &AccountEventWriter{pool: pool, queue: make(chan events.Event, 50_000), log: log}
	w.wg.Add(1)
	go w.loop()
	return w
}

// Enqueue adds events without blocking.
func (w *AccountEventWriter) Enqueue(evs []events.Event) {
	for _, e := range evs {
		select {
		case w.queue <- e:
		default:
			telemetry.AccountEventsDropped.Inc()
		}
	}
}

// Close flushes pending rows.
func (w *AccountEventWriter) Close() {
	close(w.queue)
	w.wg.Wait()
}

func (w *AccountEventWriter) loop() {
	defer w.wg.Done()
	batch := make([]events.Event, 0, 1000)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := InsertAccountEvents(ctx, w.pool, batch); err != nil {
			w.log.Warn("account_events insert failed", "rows", len(batch), "err", err)
		}
		cancel()
		batch = batch[:0]
	}
	for {
		select {
		case e, ok := <-w.queue:
			if !ok {
				flush()
				return
			}
			batch = append(batch, e)
			if len(batch) >= cap(batch) {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

var kindNames = map[events.Kind]string{events.KindLogin: "login", events.KindDevice: "device", events.KindTransaction: "transaction"}

// InsertAccountEvents inserts events idempotently (ON CONFLICT DO NOTHING).
func InsertAccountEvents(ctx context.Context, pool *pgxpool.Pool, evs []events.Event) error {
	ids := make([]string, len(evs))
	accs := make([]string, len(evs))
	kinds := make([]string, len(evs))
	ts := make([]time.Time, len(evs))
	payloads := make([]string, len(evs))
	for i := range evs {
		e := &evs[i]
		ids[i], accs[i], kinds[i] = e.EventID, e.AccountID, kindNames[e.Kind]
		ts[i] = time.UnixMilli(e.TSMillis).UTC()
		payloads[i] = string(e.AppendJSON(nil))
	}
	_, err := pool.Exec(ctx, `INSERT INTO account_events (event_id, account_id, kind, event_ts, payload)
		SELECT * FROM unnest($1::text[], $2::text[], $3::text[], $4::timestamptz[], $5::jsonb[])
		ON CONFLICT (event_id) DO NOTHING`, ids, accs, kinds, ts, payloads)
	return err
}
