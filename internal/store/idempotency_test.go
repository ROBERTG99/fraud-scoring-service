//go:build integration

package store

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/testutil"
)

func TestRedisIdempotencyFirstWriteWins(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: testutil.RedisAddr(t)})
	defer func() { _ = rdb.Close() }()
	idem := NewRedisIdempotency(rdb, testutil.UniqueName("t"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if d, err := idem.Get(ctx, "tx1"); d != nil || err != nil {
		t.Fatalf("miss: %v %v", d, err)
	}
	first := &fraudv1.Decision{DecisionId: "d1", TransactionId: "tx1", Action: fraudv1.Action_ACTION_REVIEW, ReasonCodes: []string{"R"}}
	if err := idem.Put(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := idem.Put(ctx, &fraudv1.Decision{DecisionId: "d2", TransactionId: "tx1"}); err != nil {
		t.Fatal(err)
	}
	got, err := idem.Get(ctx, "tx1")
	if err != nil || got.DecisionId != "d1" || got.ReasonCodes[0] != "R" {
		t.Fatalf("got %v %v", got, err)
	}
	ttl := rdb.TTL(ctx, idem.key("tx1")).Val()
	if ttl < 23*time.Hour || ttl > IdempotencyTTL {
		t.Fatalf("ttl %v", ttl)
	}
}
