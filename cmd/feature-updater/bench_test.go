//go:build integration

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/ROBERTG99/fraud-scoring-service/internal/events"
	"github.com/ROBERTG99/fraud-scoring-service/internal/features"
	"github.com/ROBERTG99/fraud-scoring-service/internal/stream"
	"github.com/ROBERTG99/fraud-scoring-service/internal/testutil"
)

// minUpdaterEventsPerSec is NFR-3.
const minUpdaterEventsPerSec = 10_000

func syntheticRecords(n int) []*kgo.Record {
	recs := make([]*kgo.Record, 0, n)
	base := int64(1_788_000_000_000)
	for i := 0; i < n; i++ {
		acc := fmt.Sprintf("acc_%08d", i%2000)
		sum := sha256.Sum256([]byte(fmt.Sprintf("dev-%d", i%5000)))
		e := events.Event{EventID: fmt.Sprintf("0192f000-0000-7000-8000-%012d", i), AccountID: acc, TSMillis: base + int64(i)*10,
			DeviceID: hex.EncodeToString(sum[:]), IPPrefix: "203.0.113.0/24", Country: "US"}
		switch i % 3 {
		case 0:
			e.Kind, e.Success, e.Method = events.KindLogin, true, "password"
		case 1:
			e.Kind, e.Change = events.KindDevice, events.ChangeDeviceBound
		default:
			e.Kind, e.AmountMinor, e.Currency, e.MerchantID, e.MCC, e.Channel = events.KindTransaction, 1234, "USD", "m_00001", "5411", "CARD_PRESENT"
		}
		recs = append(recs, &kgo.Record{Topic: e.TopicFor(), Key: []byte(acc), Value: e.AppendJSON(nil)})
	}
	return recs
}

func BenchmarkUpdater(b *testing.B) {
	const n = 60_000
	recs := syntheticRecords(n)
	rdb := redis.NewClient(&redis.Options{Addr: testutil.RedisAddr(b), PoolSize: 64})
	defer rdb.Close()
	var best float64
	for i := 0; i < b.N; i++ {
		cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(stream.Partitions, stream.AllTopics...))
		if err != nil {
			b.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		prod, err := kgo.NewClient(kgo.SeedBrokers(cluster.ListenAddrs()...), kgo.ProducerBatchMaxBytes(4<<20))
		if err != nil {
			b.Fatal(err)
		}
		fresh := make([]*kgo.Record, len(recs))
		for j, r := range recs {
			fresh[j] = &kgo.Record{Topic: r.Topic, Key: r.Key, Value: r.Value}
		}
		if err := prod.ProduceSync(ctx, fresh...).FirstErr(); err != nil {
			b.Fatal(err)
		}
		prod.Close()
		store := features.NewStore(rdb, testutil.UniqueName("b"), nil)
		u, err := New(Config{Brokers: cluster.ListenAddrs(), Group: "bench"}, store, multiSink{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		start := time.Now()
		done := make(chan error, 1)
		go func() { done <- u.Run(ctx) }()
		for u.Applied() < n && ctx.Err() == nil {
			time.Sleep(time.Millisecond)
		}
		elapsed := time.Since(start)
		b.StopTimer()
		cancel()
		<-done
		cluster.Close()
		if u.Applied() < n {
			b.Fatalf("applied %d of %d events", u.Applied(), n)
		}
		best = max(best, float64(n)/elapsed.Seconds())
	}
	b.ReportMetric(best, "events/s")
	if best < minUpdaterEventsPerSec {
		b.Fatalf("feature-updater throughput %.0f events/s < %d (NFR-3)", best, minUpdaterEventsPerSec)
	}
}
