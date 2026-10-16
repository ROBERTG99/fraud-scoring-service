package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
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
	<-done
}
