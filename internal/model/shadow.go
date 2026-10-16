package model

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ROBERTG99/fraud-scoring-service/internal/telemetry"
)

// Comparison is one primary-vs-shadow result (shadow_comparisons row).
type Comparison struct {
	DecisionID     string
	PrimaryVersion string
	ShadowVersion  string
	PrimaryScore   float32
	ShadowScore    float32
	PrimaryBand    string
	ShadowBand     string
	Agree          bool
	CreatedAt      time.Time
}

// ComparisonSink stores comparisons (internal/store.ShadowStore).
type ComparisonSink interface {
	InsertComparison(ctx context.Context, c Comparison) error
}

// ShadowConfig sizes the runner (spec §4.7: 16 workers, queue 2,000).
type ShadowConfig struct {
	Workers int
	Queue   int
	Timeout time.Duration
}

// ShadowRunner scores the shadow alias asynchronously. Submit never blocks: when the queue is full the job
// is dropped and counted (fraud_shadow_dropped_total). Nothing it does can change a decision.
type ShadowRunner struct {
	client  Client
	sink    ComparisonSink
	band    func(float64) string
	log     *slog.Logger
	timeout time.Duration
	jobs    chan ShadowJob
	wg      sync.WaitGroup
	dropped atomic.Int64
	done    atomic.Int64
	failed  atomic.Int64
}

// NewShadowRunner starts the workers; band maps a score to APPROVE|REVIEW|DECLINE (active thresholds).
func NewShadowRunner(client Client, sink ComparisonSink, band func(float64) string, cfg ShadowConfig, log *slog.Logger) *ShadowRunner {
	if cfg.Workers <= 0 {
		cfg.Workers = 16
	}
	if cfg.Queue <= 0 {
		cfg.Queue = 2_000
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 100 * time.Millisecond
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	r := &ShadowRunner{client: client, sink: sink, band: band, log: log, timeout: cfg.Timeout, jobs: make(chan ShadowJob, cfg.Queue)}
	for i := 0; i < cfg.Workers; i++ {
		r.wg.Add(1)
		go r.work()
	}
	return r
}

// Submit enqueues a job or drops it.
func (r *ShadowRunner) Submit(job ShadowJob) {
	if !job.PrimaryOK {
		return // nothing to compare against when the primary model was unavailable
	}
	select {
	case r.jobs <- job:
	default:
		r.dropped.Add(1)
		telemetry.ShadowDropped.Inc()
	}
}

// Close stops accepting jobs and waits for in-flight ones.
func (r *ShadowRunner) Close() {
	close(r.jobs)
	r.wg.Wait()
}

// Stats returns (completed, failed, dropped).
func (r *ShadowRunner) Stats() (int64, int64, int64) {
	return r.done.Load(), r.failed.Load(), r.dropped.Load()
}

func (r *ShadowRunner) work() {
	defer r.wg.Done()
	for job := range r.jobs {
		r.run(job)
	}
}

func (r *ShadowRunner) run(job ShadowJob) {
	defer func() {
		if p := recover(); p != nil {
			r.failed.Add(1)
			r.log.Error("shadow job panicked", "panic", p)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	pred, err := r.client.Predict(ctx, AliasShadow, [][]float32{job.Features})
	if err != nil || len(pred.Scores) != 1 {
		r.failed.Add(1)
		return
	}
	c := Comparison{DecisionID: job.DecisionID, PrimaryVersion: job.PrimaryVersion, ShadowVersion: pred.Version,
		PrimaryScore: job.PrimaryScore, ShadowScore: pred.Scores[0], PrimaryBand: r.band(float64(job.PrimaryScore)),
		ShadowBand: r.band(float64(pred.Scores[0])), CreatedAt: time.Now().UTC()}
	c.Agree = c.PrimaryBand == c.ShadowBand
	if r.sink != nil {
		if err := r.sink.InsertComparison(ctx, c); err != nil {
			r.failed.Add(1)
			r.log.Debug("shadow comparison not stored", "err", err)
			return
		}
	}
	telemetry.ShadowCompared.WithLabelValues(map[bool]string{true: "true", false: "false"}[c.Agree]).Inc()
	r.done.Add(1)
}
