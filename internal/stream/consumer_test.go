package stream

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
)

// TestRunReturnsWhileHandlerKeepsFailing: a permanently failing handler (Redis down) must not block
// shutdown, even when more batches arrive than the worker buffer holds.
func TestRunReturnsWhileHandlerKeepsFailing(t *testing.T) {
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, TopicLogin))
	if err != nil {
		t.Fatal(err)
	}
	defer cluster.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	prod, err := kgo.NewClient(kgo.SeedBrokers(cluster.ListenAddrs()...), kgo.ProducerLinger(0))
	if err != nil {
		t.Fatal(err)
	}
	defer prod.Close()
	for i := 0; i < 50; i++ { // many small batches
		if err := prod.ProduceSync(ctx, &kgo.Record{Topic: TopicLogin, Value: []byte("{}")}).FirstErr(); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int64
	failing := func(context.Context, string, int32, []*kgo.Record) error {
		calls.Add(1)
		return errors.New("redis down")
	}
	pc, err := NewPartitionConsumer(cluster.ListenAddrs(), "g", []string{TopicLogin}, failing,
		slog.New(slog.NewTextHandler(io.Discard, nil)), kgo.FetchMaxBytes(64), kgo.FetchMaxPartitionBytes(64))
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- pc.Run(runCtx) }()
	deadline := time.Now().Add(10 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() < 2 {
		t.Fatal("handler was not retried")
	}
	stop()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel while the handler kept failing")
	}
}
