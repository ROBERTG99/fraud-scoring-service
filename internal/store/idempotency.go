package store

import (
	"bytes"
	"context"
	"crypto/sha256"
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

// idemMagic prefixes values that carry the request hash: magic, 32-byte SHA-256, then the decision protobuf.
// Values without it (written before the hash existed) are a bare decision.
var idemMagic = []byte("fidem1")

// Get returns the stored decision and its request hash (nil for a value without one), or nils on a miss.
func (r *RedisIdempotency) Get(ctx context.Context, txID string) (*fraudv1.Decision, []byte, error) {
	b, err := r.rdb.Get(ctx, r.key(txID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var hash []byte
	if bytes.HasPrefix(b, idemMagic) && len(b) >= len(idemMagic)+sha256.Size {
		hash = b[len(idemMagic) : len(idemMagic)+sha256.Size]
		b = b[len(idemMagic)+sha256.Size:]
	}
	var d fraudv1.Decision
	if err := proto.Unmarshal(b, &d); err != nil {
		return nil, nil, err
	}
	return &d, hash, nil
}

// Put stores the decision and its request hash unless one is already stored (SET NX, 24 h TTL).
func (r *RedisIdempotency) Put(ctx context.Context, d *fraudv1.Decision, requestHash []byte) error {
	b, err := proto.Marshal(d)
	if err != nil {
		return err
	}
	if len(requestHash) == sha256.Size {
		b = append(append(append([]byte{}, idemMagic...), requestHash...), b...)
	}
	return r.rdb.SetNX(ctx, r.key(d.TransactionId), b, IdempotencyTTL).Err()
}
