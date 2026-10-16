package stream

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

// BatchHandler processes the records of one partition, in offset order. Returning an error makes the
// consumer retry the same batch (with backoff) until it succeeds or the context ends: at-least-once.
type BatchHandler func(ctx context.Context, topic string, partition int32, recs []*kgo.Record) error

type tp struct {
	topic     string
	partition int32
}

type worker struct {
	recs chan []*kgo.Record
	quit chan struct{}
	done chan struct{}
}

// PartitionConsumer runs one goroutine per assigned partition (franz-go goroutine-per-partition
// pattern). Offsets are committed only after the handler succeeds.
type PartitionConsumer struct {
	cl      *kgo.Client
	handle  BatchHandler
	log     *slog.Logger
	mu      sync.Mutex
	workers map[tp]*worker
	ctx     context.Context
}

// NewPartitionConsumer builds a consumer-group client for topics.
func NewPartitionConsumer(brokers []string, group string, topics []string, handle BatchHandler, log *slog.Logger, extra ...kgo.Opt) (*PartitionConsumer, error) {
	pc := &PartitionConsumer{handle: handle, log: log, workers: map[tp]*worker{}}
	opts := append([]kgo.Opt{
		kgo.SeedBrokers(brokers...),
		kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(topics...),
		kgo.DisableAutoCommit(),
		kgo.BlockRebalanceOnPoll(),
		kgo.OnPartitionsAssigned(pc.assigned),
		kgo.OnPartitionsRevoked(pc.revoked),
		kgo.OnPartitionsLost(pc.revoked),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.FetchMaxWait(200 * time.Millisecond),
	}, extra...)
	cl, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, err
	}
	pc.cl = cl
	return pc, nil
}

// Client exposes the underlying client (for producing DLQ records).
func (pc *PartitionConsumer) Client() *kgo.Client { return pc.cl }

// Run polls until ctx ends, then stops workers and closes the client.
func (pc *PartitionConsumer) Run(ctx context.Context) error {
	pc.mu.Lock()
	pc.ctx = ctx
	pc.mu.Unlock()
	defer pc.cl.Close()
	for {
		fetches := pc.cl.PollRecords(ctx, 10_000)
		if fetches.IsClientClosed() || ctx.Err() != nil {
			pc.stopAll()
			// With BlockRebalanceOnPoll, Close waits for AllowRebalance; release it before returning.
			pc.cl.AllowRebalance()
			return ctx.Err()
		}
		fetches.EachError(func(t string, p int32, err error) {
			if !errors.Is(err, context.Canceled) {
				pc.log.Warn("fetch error", "topic", t, "partition", p, "err", err)
			}
		})
		fetches.EachPartition(func(p kgo.FetchTopicPartition) {
			if len(p.Records) == 0 {
				return
			}
			pc.mu.Lock()
			w := pc.workers[tp{p.Topic, p.Partition}]
			pc.mu.Unlock()
			if w != nil {
				// Never block shutdown on a worker stuck retrying (for example while Redis is down).
				select {
				case w.recs <- p.Records:
				case <-w.done:
				case <-ctx.Done():
				}
			}
		})
		pc.cl.AllowRebalance()
	}
}

func (pc *PartitionConsumer) assigned(_ context.Context, _ *kgo.Client, assigned map[string][]int32) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	for topic, parts := range assigned {
		for _, p := range parts {
			w := &worker{recs: make(chan []*kgo.Record, 4), quit: make(chan struct{}), done: make(chan struct{})}
			pc.workers[tp{topic, p}] = w
			go pc.work(topic, p, w)
		}
	}
}

func (pc *PartitionConsumer) revoked(_ context.Context, _ *kgo.Client, revoked map[string][]int32) {
	pc.mu.Lock()
	var stopping []*worker
	for topic, parts := range revoked {
		for _, p := range parts {
			key := tp{topic, p}
			if w := pc.workers[key]; w != nil {
				close(w.quit)
				stopping = append(stopping, w)
				delete(pc.workers, key)
			}
		}
	}
	pc.mu.Unlock()
	for _, w := range stopping {
		<-w.done
	}
}

func (pc *PartitionConsumer) stopAll() {
	pc.mu.Lock()
	ws := make([]*worker, 0, len(pc.workers))
	for k, w := range pc.workers {
		close(w.quit)
		ws = append(ws, w)
		delete(pc.workers, k)
	}
	pc.mu.Unlock()
	for _, w := range ws {
		<-w.done
	}
}

func (pc *PartitionConsumer) work(topic string, partition int32, w *worker) {
	defer close(w.done)
	pc.mu.Lock()
	ctx := pc.ctx
	pc.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		select {
		case <-w.quit:
			return
		case recs := <-w.recs:
			backoff := 50 * time.Millisecond
			for {
				err := pc.handle(ctx, topic, partition, recs)
				if err == nil {
					break
				}
				pc.log.Warn("batch failed, retrying", "topic", topic, "partition", partition, "err", err)
				select {
				case <-w.quit:
					return
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				backoff = min(2*backoff, 5*time.Second)
			}
			cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := pc.cl.CommitRecords(cctx, recs...); err != nil {
				pc.log.Warn("commit failed", "topic", topic, "partition", partition, "err", err)
			}
			cancel()
		}
	}
}

// DLQMessage is the fraud.dlq.v1 payload.
type DLQMessage struct {
	Schema      string `json:"schema"`
	SourceTopic string `json:"source_topic"`
	Partition   int32  `json:"partition"`
	Offset      int64  `json:"offset"`
	Error       string `json:"error"`
	RawB64      string `json:"raw_b64"`
	FailedAt    string `json:"failed_at"`
}

// DLQRecord builds the events.dlq record for an invalid message.
func DLQRecord(r *kgo.Record, cause error, now time.Time) *kgo.Record {
	msg := cause.Error()
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	body, _ := json.Marshal(DLQMessage{
		Schema: SchemaDLQ, SourceTopic: r.Topic, Partition: r.Partition, Offset: r.Offset, Error: msg,
		RawB64: base64.StdEncoding.EncodeToString(r.Value), FailedAt: now.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
	})
	return &kgo.Record{Topic: TopicDLQ, Key: r.Key, Value: body, Headers: r.Headers}
}
