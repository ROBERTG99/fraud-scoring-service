package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/ROBERTG99/fraud-scoring-service/internal/events"
	"github.com/ROBERTG99/fraud-scoring-service/internal/stream"
	"github.com/ROBERTG99/fraud-scoring-service/internal/telemetry"
)

// Applier applies valid events to the online feature store (features.Store in production).
type Applier interface {
	ApplyEvents(ctx context.Context, evs []events.Event) error
}

// Sink receives applied events for account timelines (store.AccountEventWriter in production).
type Sink interface {
	Enqueue(evs []events.Event)
}

// Config for the updater.
type Config struct {
	Brokers []string
	Group   string
	Now     func() time.Time
}

// Updater consumes events.* and keeps Redis features current; invalid messages go to events.dlq.
type Updater struct {
	cfg      Config
	applier  Applier
	sink     Sink
	log      *slog.Logger
	consumer *stream.PartitionConsumer
	valid    *stream.Validator
	applied  atomic.Int64
	lastTS   atomic.Int64
}

// UpdaterTopics are the topics the feature-updater consumes.
var UpdaterTopics = []string{stream.TopicLogin, stream.TopicDevice, stream.TopicTransaction}

// New builds an updater; Run starts consuming.
func New(cfg Config, applier Applier, sink Sink, log *slog.Logger) (*Updater, error) {
	if cfg.Group == "" {
		cfg.Group = "feature-updater"
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	u := &Updater{cfg: cfg, applier: applier, sink: sink, log: log, valid: stream.MustValidator()}
	pc, err := stream.NewPartitionConsumer(cfg.Brokers, cfg.Group, UpdaterTopics, u.handle, log,
		kgo.ProducerLinger(0), kgo.RecordPartitioner(kgo.StickyKeyPartitioner(nil)))
	if err != nil {
		return nil, err
	}
	u.consumer = pc
	return u, nil
}

// Run consumes until ctx ends.
func (u *Updater) Run(ctx context.Context) error { return u.consumer.Run(ctx) }

// Applied returns the number of events applied so far.
func (u *Updater) Applied() int64 { return u.applied.Load() }

// handle validates a partition batch, sends invalid records to the DLQ, then applies the rest in one
// pipelined Redis round trip. The consumer commits offsets only after this returns nil.
func (u *Updater) handle(ctx context.Context, topic string, partition int32, recs []*kgo.Record) error {
	valid := make([]events.Event, 0, len(recs))
	var dlq []*kgo.Record
	now := u.cfg.Now()
	for _, r := range recs {
		if err := u.valid.ValidateTopic(topic, r.Value); err != nil {
			dlq = append(dlq, stream.DLQRecord(r, err, now))
			continue
		}
		var e events.Event
		if err := json.Unmarshal(r.Value, &e); err != nil {
			dlq = append(dlq, stream.DLQRecord(r, err, now))
			continue
		}
		valid = append(valid, e)
	}
	if len(dlq) > 0 {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := u.consumer.Client().ProduceSync(pctx, dlq...).FirstErr()
		cancel()
		if err != nil {
			return fmt.Errorf("produce dlq: %w", err)
		}
		telemetry.DLQTotal.WithLabelValues(topic).Add(float64(len(dlq)))
		telemetry.UpdaterEvents.WithLabelValues(topic, "dlq").Add(float64(len(dlq)))
		u.log.Info("routed invalid events to dlq", "topic", topic, "partition", partition, "count", len(dlq))
	}
	if len(valid) == 0 {
		return nil
	}
	actx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := u.applier.ApplyEvents(actx, valid); err != nil {
		return err
	}
	u.sink.Enqueue(valid)
	u.applied.Add(int64(len(valid)))
	telemetry.UpdaterEvents.WithLabelValues(topic, "applied").Add(float64(len(valid)))
	last := recs[len(recs)-1]
	lag := now.Sub(last.Timestamp).Seconds()
	telemetry.UpdaterLag.WithLabelValues(strconv.Itoa(int(partition))).Set(max(0, lag))
	u.lastTS.Store(last.Timestamp.UnixMilli())
	return nil
}
