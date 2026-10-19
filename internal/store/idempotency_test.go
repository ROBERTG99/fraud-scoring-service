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
	idem := NewRedisIdempotency(rdb, testutil.UniqueName("t"), testIdemKey)
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
	// A value without the request hash is never served (M6 review): whoever reaches Redis could plant a bare
	// APPROVE for a future transaction id. It reads as a miss, so Score decides, and Postgres's ON CONFLICT
	// still returns a decision that really was stored.
	planted, _ := proto.Marshal(&fraudv1.Decision{DecisionId: "d3", TransactionId: "tx3", Action: fraudv1.Action_ACTION_APPROVE})
	if err := rdb.Set(ctx, idem.redisKey("tx3"), planted, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if d, h, err := idem.Get(ctx, "tx3"); err != nil || d != nil || h != nil {
		t.Fatalf("an unprefixed value was served: %v %x %v", d, h, err)
	}
	// It is removed on read, so the real decision's Put (SET NX) is not blocked for the planted value's TTL.
	if err := idem.Put(ctx, &fraudv1.Decision{DecisionId: "d3-real", TransactionId: "tx3"}, hash); err != nil {
		t.Fatal(err)
	}
	if d, _, err := idem.Get(ctx, "tx3"); err != nil || d == nil || d.DecisionId != "d3-real" {
		t.Fatalf("after a planted value, the real decision: %v %v", d, err)
	}
	if err := idem.Put(ctx, &fraudv1.Decision{DecisionId: "d4", TransactionId: "tx4"}, nil); err == nil {
		t.Fatal("Put without a request hash must fail: it would write a value nobody can read")
	}
	ttl := rdb.TTL(ctx, idem.redisKey("tx1")).Val()
	if ttl < 23*time.Hour || ttl > IdempotencyTTL {
		t.Fatalf("ttl %v", ttl)
	}
}

var testIdemKey = []byte("idempotency key for tests, 32 b!")

// Security audit round 3: with the plain request hash, whoever reaches Redis could rewrite a stored decision
// (REVIEW to APPROVE, keeping the hash) or plant one under a future transaction_id. Values are MACed under a
// server key now: an edited or planted value reads as a miss and is removed.
func TestRedisIdempotencyRejectsForgedValues(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: testutil.RedisAddr(t)})
	defer func() { _ = rdb.Close() }()
	idem := NewRedisIdempotency(rdb, testutil.UniqueName("f"), testIdemKey)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hash := bytes.Repeat([]byte{3}, 32)
	if err := idem.Put(ctx, &fraudv1.Decision{DecisionId: "d1", TransactionId: "tx1", Action: fraudv1.Action_ACTION_REVIEW}, hash); err != nil {
		t.Fatal(err)
	}
	raw, err := rdb.Get(ctx, idem.redisKey("tx1")).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	approve, _ := proto.Marshal(&fraudv1.Decision{DecisionId: "d1", TransactionId: "tx1", Action: fraudv1.Action_ACTION_APPROVE})
	edited := append(append([]byte{}, raw[:len(raw)-len(approve)]...), approve...) // same header, other decision
	planted := append(append(append([]byte{}, idemMagic...), make([]byte, 64)...), approve...)
	for name, v := range map[string][]byte{"edited": edited, "planted": planted} {
		if err := rdb.Set(ctx, idem.redisKey("tx1"), v, time.Minute).Err(); err != nil {
			t.Fatal(err)
		}
		if d, h, err := idem.Get(ctx, "tx1"); err != nil || d != nil || h != nil {
			t.Fatalf("%s value served: %v %x %v", name, d, h, err)
		}
		if n := rdb.Exists(ctx, idem.redisKey("tx1")).Val(); n != 0 {
			t.Fatalf("%s value left in place", name)
		}
	}
	// The MAC binds the transaction_id and the request hash (M6 review round 4): a genuine value copied to
	// another transaction's key, or with its request hash changed, is not served and is removed.
	if err := idem.Put(ctx, &fraudv1.Decision{DecisionId: "d1", TransactionId: "tx1", Action: fraudv1.Action_ACTION_REVIEW}, hash); err != nil {
		t.Fatal(err)
	}
	rehashed := append([]byte{}, raw...)
	copy(rehashed[6:6+32], bytes.Repeat([]byte{4}, 32))
	for name, kv := range map[string]struct {
		tx string
		v  []byte
	}{"copied": {"tx9", raw}, "rehashed": {"tx1", rehashed}} {
		if err := rdb.Set(ctx, idem.redisKey(kv.tx), kv.v, time.Minute).Err(); err != nil {
			t.Fatal(err)
		}
		if d, h, err := idem.Get(ctx, kv.tx); err != nil || d != nil || h != nil {
			t.Fatalf("%s value served: %v %x %v", name, d, h, err)
		}
		if n := rdb.Exists(ctx, idem.redisKey(kv.tx)).Val(); n != 0 {
			t.Fatalf("%s value left in place", name)
		}
	}
	other := NewRedisIdempotency(rdb, testutil.UniqueName("f"), []byte("a different key, also 32 bytes!!"))
	if err := other.Put(ctx, &fraudv1.Decision{DecisionId: "d2", TransactionId: "tx2"}, hash); err != nil {
		t.Fatal(err)
	}
	moved, _ := rdb.Get(ctx, other.redisKey("tx2")).Bytes()
	_ = rdb.Set(ctx, idem.redisKey("tx2"), moved, time.Minute).Err()
	if d, _, _ := idem.Get(ctx, "tx2"); d != nil {
		t.Fatal("a value MACed under another key was served")
	}
}
