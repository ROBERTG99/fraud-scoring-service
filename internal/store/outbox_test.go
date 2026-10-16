//go:build integration

package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/stream"
)

// FR-10: the relay crashes after producing but before marking rows sent; on restart the rows are sent
// again, the duplicates carry the same decision_id, and a consumer deduplicating on decision_id
// (processed_messages) processes each decision once.
func TestRelayCrashProducesDuplicatesWithSameDecisionID(t *testing.T) {
	pool := newPool(t, migratedDB(t))
	w, err := NewGroupWriter(pool, WriterConfig{SpoolDir: t.TempDir()}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const n = 5
	for i := 0; i < n; i++ {
		if _, err := w.Commit(ctx, record(3000+i, fraudv1.Action_ACTION_REVIEW)); err != nil {
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

	relay := NewRelay(pool, prod, quiet, 100)
	relay.afterProduce = func() error { return errors.New("simulated crash before marking sent") }
	if _, err := relay.RunOnce(ctx); err == nil {
		t.Fatal("expected the simulated crash")
	}
	if got := count(t, pool, "SELECT count(*) FROM decision_outbox WHERE sent_at IS NULL"); got != n {
		t.Fatalf("after the crash %d rows unsent, want %d", got, n)
	}
	relay.afterProduce = nil // restart
	sent, err := relay.RunOnce(ctx)
	if err != nil || sent != n {
		t.Fatalf("restart sent %d, %v", sent, err)
	}
	if again, _ := relay.RunOnce(ctx); again != 0 {
		t.Fatalf("rows sent twice after marking: %d", again)
	}

	cons, err := kgo.NewClient(kgo.SeedBrokers(cluster.ListenAddrs()...), kgo.ConsumeTopics(TopicDecisions), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	defer cons.Close()
	byDecision := map[string]int{}
	total := 0
	for total < 2*n && ctx.Err() == nil {
		cons.PollFetches(ctx).EachRecord(func(r *kgo.Record) {
			if err := stream.MustValidator().ValidateTopic(stream.TopicDecisions, r.Value); err != nil {
				t.Errorf("decision message invalid: %v", err)
			}
			var m DecisionMessage
			_ = json.Unmarshal(r.Value, &m)
			byDecision[m.DecisionID]++
			total++
			if string(r.Key) != "acc_00000042" {
				t.Errorf("key %q, want account_id", r.Key)
			}
			// Consumer-side deduplication, as the case summarizer does.
			if _, err := pool.Exec(ctx, `INSERT INTO processed_messages (consumer, message_key) VALUES ('test', $1) ON CONFLICT DO NOTHING`, m.DecisionID); err != nil {
				t.Error(err)
			}
		})
	}
	if total != 2*n || len(byDecision) != n {
		t.Fatalf("got %d messages for %d decisions, want %d for %d", total, len(byDecision), 2*n, n)
	}
	for id, c := range byDecision {
		if c != 2 {
			t.Fatalf("decision %s seen %d times, want 2 (one duplicate)", id, c)
		}
	}
	if got := count(t, pool, "SELECT count(*) FROM processed_messages WHERE consumer = 'test'"); got != n {
		t.Fatalf("consumer processed %d unique decisions, want %d", got, n)
	}
}
