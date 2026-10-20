package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/ROBERTG99/fraud-scoring-service/internal/events"
	"github.com/ROBERTG99/fraud-scoring-service/internal/stream"
)

type fakeApplier struct {
	mu  sync.Mutex
	evs []events.Event
}

func (f *fakeApplier) ApplyEvents(_ context.Context, evs []events.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evs = append(f.evs, evs...)
	return nil
}

func (f *fakeApplier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.evs)
}

type nopSink struct{}

func (nopSink) Enqueue([]events.Event) {}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "tests", "fixtures", "events", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestUpdaterRoutesInvalidToDLQ(t *testing.T) {
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(stream.Partitions, stream.AllTopics...))
	if err != nil {
		t.Fatal(err)
	}
	defer cluster.Close()
	brokers := cluster.ListenAddrs()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	prod, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		t.Fatal(err)
	}
	defer prod.Close()
	valid := fixture(t, "valid/login_01.json")
	invalid := fixture(t, "invalid/login_missing_method.json")
	key := []byte("acc_000123")
	res := prod.ProduceSync(ctx,
		&kgo.Record{Topic: stream.TopicLogin, Key: key, Value: valid},
		&kgo.Record{Topic: stream.TopicLogin, Key: key, Value: invalid},
		&kgo.Record{Topic: stream.TopicLogin, Key: key, Value: []byte("{not json")},
	)
	if err := res.FirstErr(); err != nil {
		t.Fatal(err)
	}
	invalidOffset := res[1].Record.Offset
	partition := res[1].Record.Partition

	applier := &fakeApplier{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	u, err := New(Config{Brokers: brokers, Group: "updater-test"}, applier, nopSink{}, log)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- u.Run(runCtx) }()

	// Poll the DLQ with a deadline instead of sleeping.
	dlq, err := kgo.NewClient(kgo.SeedBrokers(brokers...), kgo.ConsumeTopics(stream.TopicDLQ), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	defer dlq.Close()
	var got []stream.DLQMessage
	for len(got) < 2 && ctx.Err() == nil {
		fetches := dlq.PollFetches(ctx)
		fetches.EachRecord(func(r *kgo.Record) {
			var m stream.DLQMessage
			if err := json.Unmarshal(r.Value, &m); err != nil {
				t.Errorf("dlq payload: %v", err)
			}
			if err := stream.MustValidator().ValidateTopic(stream.TopicDLQ, r.Value); err != nil {
				t.Errorf("dlq payload does not match fraud.dlq.v1: %v", err)
			}
			got = append(got, m)
		})
	}
	if len(got) != 2 {
		t.Fatalf("got %d DLQ messages, want 2", len(got))
	}
	if got[0].SourceTopic != stream.TopicLogin || got[0].Offset != invalidOffset || got[0].Partition != partition {
		t.Fatalf("unexpected DLQ metadata %+v (want offset %d partition %d)", got[0], invalidOffset, partition)
	}
	if got[0].Error == "" || got[0].RawB64 == "" {
		t.Fatalf("DLQ message must carry error and raw payload: %+v", got[0])
	}
	// The valid event was applied; offsets are committed past the invalid ones.
	deadline := time.Now().Add(10 * time.Second)
	for applier.count() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if applier.count() != 1 {
		t.Fatalf("applied %d events, want 1", applier.count())
	}
	adm, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		t.Fatal(err)
	}
	defer adm.Close()
	committedPast := func() bool {
		offsets := fetchCommitted(ctx, t, adm, "updater-test")
		return offsets[partition] >= res[2].Record.Offset+1
	}
	for !committedPast() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !committedPast() {
		t.Fatal("offset was not committed past the invalid messages")
	}
	stop()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("run: %v", err)
	}
}

type flakyApplier struct {
	fakeApplier
	failures atomic.Int64
}

func (f *flakyApplier) ApplyEvents(ctx context.Context, evs []events.Event) error {
	if f.failures.Add(-1) >= 0 {
		return errors.New("redis down")
	}
	return f.fakeApplier.ApplyEvents(ctx, evs)
}

// TestRetriedBatchDoesNotDuplicateDLQ: when applying fails and the batch is retried, invalid
// messages already routed to the DLQ are not published again.
func TestRetriedBatchDoesNotDuplicateDLQ(t *testing.T) {
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(stream.Partitions, stream.AllTopics...))
	if err != nil {
		t.Fatal(err)
	}
	defer cluster.Close()
	brokers := cluster.ListenAddrs()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	prod, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		t.Fatal(err)
	}
	defer prod.Close()
	key := []byte("acc_000123")
	if err := prod.ProduceSync(ctx,
		&kgo.Record{Topic: stream.TopicLogin, Key: key, Value: fixture(t, "invalid/login_bad_method.json")},
		&kgo.Record{Topic: stream.TopicLogin, Key: key, Value: fixture(t, "valid/login_02.json")},
	).FirstErr(); err != nil {
		t.Fatal(err)
	}
	applier := &flakyApplier{}
	applier.failures.Store(3)
	u, err := New(Config{Brokers: brokers, Group: "retry-test"}, applier, nopSink{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- u.Run(runCtx) }()
	deadline := time.Now().Add(20 * time.Second)
	for applier.count() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("run: %v", err)
	}
	if applier.count() != 1 {
		t.Fatalf("applied %d events after retries, want 1", applier.count())
	}
	dlq, err := kgo.NewClient(kgo.SeedBrokers(brokers...), kgo.ConsumeTopics(stream.TopicDLQ), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	defer dlq.Close()
	// Wait for the first DLQ record (a loaded runner can take seconds to fetch), then keep reading for 2 s
	// more so that a duplicate from a retried batch would be counted.
	n := 0
	firstCtx, fcancel := context.WithTimeout(ctx, 20*time.Second)
	defer fcancel()
	for n == 0 && firstCtx.Err() == nil {
		n += dlq.PollFetches(firstCtx).NumRecords()
	}
	pollCtx, pcancel := context.WithTimeout(ctx, 2*time.Second)
	defer pcancel()
	for pollCtx.Err() == nil {
		n += dlq.PollFetches(pollCtx).NumRecords()
	}
	if n != 1 {
		t.Fatalf("DLQ has %d messages after retries, want exactly 1", n)
	}
}
