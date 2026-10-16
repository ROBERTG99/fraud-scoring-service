package store

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
)

// IdempotencyTTL is how long a decision is replayable from Redis (spec §4.4).
const IdempotencyTTL = 24 * time.Hour

// RedisIdempotency stores serialized decisions at fr:idem:score:{transaction_id} with SET NX.
type RedisIdempotency struct {
	rdb    redis.Cmdable
	prefix string
}

// NewRedisIdempotency returns the store; prefix defaults to "fr".
func NewRedisIdempotency(rdb redis.Cmdable, prefix string) *RedisIdempotency {
	if prefix == "" {
		prefix = "fr"
	}
	return &RedisIdempotency{rdb: rdb, prefix: prefix}
}

func (r *RedisIdempotency) key(txID string) string { return r.prefix + ":idem:score:" + txID }

// Get returns the stored decision, or (nil, nil) on a miss.
func (r *RedisIdempotency) Get(ctx context.Context, txID string) (*fraudv1.Decision, error) {
	b, err := r.rdb.Get(ctx, r.key(txID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var d fraudv1.Decision
	if err := proto.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// Put stores the decision unless one is already stored (SET NX, 24 h TTL).
func (r *RedisIdempotency) Put(ctx context.Context, d *fraudv1.Decision) error {
	b, err := proto.Marshal(d)
	if err != nil {
		return err
	}
	return r.rdb.SetNX(ctx, r.key(d.TransactionId), b, IdempotencyTTL).Err()
}
