//go:build integration

package store

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/testutil"
)

func TestRedisIdempotencyFirstWriteWins(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: testutil.RedisAddr(t)})
	defer func() { _ = rdb.Close() }()
	idem := NewRedisIdempotency(rdb, testutil.UniqueName("t"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if d, h, err := idem.Get(ctx, "tx1"); d != nil || h != nil || err != nil {
		t.Fatalf("miss: %v %v %v", d, h, err)
	}
	hash := bytes.Repeat([]byte{9}, 32)
	first := &fraudv1.Decision{DecisionId: "d1", TransactionId: "tx1", Action: fraudv1.Action_ACTION_REVIEW, ReasonCodes: []string{"R"}}
	if err := idem.Put(ctx, first, hash); err != nil {
		t.Fatal(err)
	}
	if err := idem.Put(ctx, &fraudv1.Decision{DecisionId: "d2", TransactionId: "tx1"}, bytes.Repeat([]byte{1}, 32)); err != nil {
		t.Fatal(err)
	}
	got, gotHash, err := idem.Get(ctx, "tx1")
	if err != nil || got.DecisionId != "d1" || got.ReasonCodes[0] != "R" || !bytes.Equal(gotHash, hash) {
		t.Fatalf("got %v %x %v", got, gotHash, err)
	}
	// A value written before request hashes existed (a bare decision) still reads, without a hash.
	legacy, _ := proto.Marshal(&fraudv1.Decision{DecisionId: "d3", TransactionId: "tx3"})
	if err := rdb.Set(ctx, idem.key("tx3"), legacy, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if d, h, err := idem.Get(ctx, "tx3"); err != nil || d.DecisionId != "d3" || h != nil {
		t.Fatalf("legacy value: %v %x %v", d, h, err)
	}
	ttl := rdb.TTL(ctx, idem.key("tx1")).Val()
	if ttl < 23*time.Hour || ttl > IdempotencyTTL {
		t.Fatalf("ttl %v", ttl)
	}
}
