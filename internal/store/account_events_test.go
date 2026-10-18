//go:build integration

package store

import (
	"context"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/events"
	"github.com/ROBERTG99/fraud-scoring-service/internal/stream"
)

// The writer batches in the background: rows appear within a flush interval, Close flushes the rest, and
// a redelivered event is not inserted twice.
func TestAccountEventWriterFlushesOnTickAndClose(t *testing.T) {
	pool := newPool(t, migratedDB(t))
	w := NewAccountEventWriter(pool, quiet)
	ts := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC).UnixMilli()
	dev := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	evs := []events.Event{
		{Kind: events.KindLogin, EventID: "ae-login", AccountID: "acc_00000042", TSMillis: ts, DeviceID: dev,
			IPPrefix: "203.0.113.0/24", Country: "US", Success: true, Method: "password"},
		{Kind: events.KindTransaction, EventID: "ae-tx", AccountID: "acc_00000042", TSMillis: ts + 1000, DeviceID: dev,
			AmountMinor: 4200, Currency: "USD", MCC: "5411", Channel: events.ChannelEcommerce},
	}
	w.Enqueue(evs)
	deadline := time.Now().Add(5 * time.Second)
	for count(t, pool, "SELECT count(*) FROM account_events") < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the ticker did not flush within 5 s")
		}
		time.Sleep(20 * time.Millisecond) // poll with a deadline
	}
	w.Enqueue([]events.Event{{Kind: events.KindLogin, EventID: "ae-login-2", AccountID: "acc_00000042", TSMillis: ts + 2000,
		DeviceID: dev, IPPrefix: "203.0.113.0/24", Country: "US", Success: false, Method: "password"}})
	w.Enqueue(evs[:1]) // redelivered
	w.Close()
	if n := count(t, pool, "SELECT count(*) FROM account_events"); n != 3 {
		t.Fatalf("after Close: %d rows, want 3", n)
	}
	if n := count(t, pool, "SELECT count(*) FROM account_events WHERE kind = 'transaction' AND payload->>'mcc' = '5411'"); n != 1 {
		t.Fatalf("transaction payload not stored: %d", n)
	}
}

// Run drains a backlog larger than one batch, keeps going on its interval, and returns when ctx ends.
func TestRelayRunDrainsInBatchesUntilCancelled(t *testing.T) {
	pool := newPool(t, migratedDB(t))
	w, err := NewGroupWriter(pool, relayWriterConfig(t), quiet)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i := range 5 {
		if _, _, err := w.Commit(ctx, record(7000+i, fraudv1.Action_ACTION_REVIEW)); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(stream.Partitions, stream.AllTopics...))
	if err != nil {
		t.Fatal(err)
	}
	defer cluster.Close()
	prod, err := kgo.NewClient(kgo.SeedBrokers(cluster.ListenAddrs()...))
	if err != nil {
		t.Fatal(err)
	}
	defer prod.Close()
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		NewRelay(pool, prod, quiet, 2).Run(runCtx, 10*time.Millisecond) // batch 2: three RunOnce calls
		close(done)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for count(t, pool, "SELECT count(*) FROM decision_outbox WHERE sent_at IS NULL") > 0 {
		if time.Now().After(deadline) {
			t.Fatal("relay did not drain the outbox")
		}
		time.Sleep(10 * time.Millisecond) // poll with a deadline
	}
	stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
