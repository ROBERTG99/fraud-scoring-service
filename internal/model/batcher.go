package model

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// BatchConfig sizes the Batcher.
type BatchConfig struct {
	MaxBatch    int // rows per model call (64)
	MaxInFlight int // concurrent model calls per alias (4); further requests queue and join the next batch
	Queue       int // pending requests per alias (4,096)
}

// ErrClosed is returned after Close.
var ErrClosed = errors.New("model batcher closed")

// Batcher coalesces concurrent single-row predictions into one model call per alias (docs/DECISIONS.md
// D-024). It never waits for a batch to fill: a request is sent at once when a call slot is free, and
// requests that arrive while all MaxInFlight calls are busy ride together in the next one. So an idle
// scorer pays no extra latency and a busy one makes far fewer HTTP calls.
type Batcher struct {
	next   Client
	cfg    BatchConfig
	mu     sync.Mutex
	queues map[string]chan *pending
	stop   chan struct{}
	wg     sync.WaitGroup
	closed bool
}

type pending struct {
	ctx  context.Context
	rows [][]float32
	done chan result
}

type result struct {
	scores  []float32
	version string
	err     error
}

// NewBatcher wraps next.
func NewBatcher(next Client, cfg BatchConfig) *Batcher {
	if cfg.MaxBatch <= 0 {
		cfg.MaxBatch = 64
	}
	if cfg.MaxInFlight <= 0 {
		cfg.MaxInFlight = 4
	}
	if cfg.Queue <= 0 {
		cfg.Queue = 4096
	}
	return &Batcher{next: next, cfg: cfg, queues: map[string]chan *pending{}, stop: make(chan struct{})}
}

func (b *Batcher) queue(alias string) (chan *pending, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, ErrClosed
	}
	q, ok := b.queues[alias]
	if !ok {
		q = make(chan *pending, b.cfg.Queue)
		b.queues[alias] = q
		b.wg.Add(1)
		go b.dispatch(alias, q)
	}
	return q, nil
}

// Predict implements Client.
func (b *Batcher) Predict(ctx context.Context, alias string, rows [][]float32) (Prediction, error) {
	if len(rows) >= b.cfg.MaxBatch {
		return b.next.Predict(ctx, alias, rows)
	}
	ctx, cancel := withDefaultDeadline(ctx)
	defer cancel()
	q, err := b.queue(alias)
	if err != nil {
		return Prediction{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	p := &pending{ctx: ctx, rows: rows, done: make(chan result, 1)}
	select {
	case q <- p:
	case <-ctx.Done():
		return Prediction{}, fmt.Errorf("%w: queue: %v", ErrUnavailable, ctx.Err())
	case <-b.stop:
		return Prediction{}, fmt.Errorf("%w: %w", ErrUnavailable, ErrClosed)
	}
	select {
	case r := <-p.done:
		if r.err != nil {
			return Prediction{}, r.err
		}
		return Prediction{Scores: r.scores, Version: r.version}, nil
	case <-ctx.Done():
		return Prediction{}, fmt.Errorf("%w: %v", ErrUnavailable, ctx.Err())
	}
}

func (b *Batcher) dispatch(alias string, q chan *pending) {
	defer b.wg.Done()
	slots := make(chan struct{}, b.cfg.MaxInFlight)
	var calls sync.WaitGroup
	defer calls.Wait()
	for {
		var first *pending
		select {
		case first = <-q:
		case <-b.stop:
			return
		}
		select {
		case slots <- struct{}{}:
		case <-b.stop:
			first.done <- result{err: fmt.Errorf("%w: %w", ErrUnavailable, ErrClosed)}
			return
		}
		batch, n := []*pending{first}, len(first.rows)
	drain:
		for n < b.cfg.MaxBatch {
			select {
			case p := <-q:
				batch = append(batch, p)
				n += len(p.rows)
			default:
				break drain
			}
		}
		calls.Add(1)
		go func() {
			defer calls.Done()
			defer func() { <-slots }()
			b.send(alias, batch)
		}()
	}
}

// send makes one model call for batch under the earliest member deadline.
func (b *Batcher) send(alias string, batch []*pending) {
	live := batch[:0]
	var deadline time.Time
	rows := make([][]float32, 0, len(batch))
	for _, p := range batch {
		if p.ctx.Err() != nil {
			continue // the caller already returned ErrUnavailable
		}
		if d, ok := p.ctx.Deadline(); ok && (deadline.IsZero() || d.Before(deadline)) {
			deadline = d
		}
		live = append(live, p)
		rows = append(rows, p.rows...)
	}
	if len(live) == 0 {
		return
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	pred, err := b.next.Predict(ctx, alias, rows)
	if err == nil && len(pred.Scores) != len(rows) {
		err = fmt.Errorf("%w: got %d scores for %d rows", ErrUnavailable, len(pred.Scores), len(rows))
	}
	off := 0
	for _, p := range live {
		if err != nil {
			p.done <- result{err: err}
			continue
		}
		p.done <- result{scores: pred.Scores[off : off+len(p.rows)], version: pred.Version}
		off += len(p.rows)
	}
}

// Close stops the dispatchers after in-flight calls finish; later calls return ErrUnavailable.
func (b *Batcher) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	close(b.stop)
	b.mu.Unlock()
	b.wg.Wait()
}
