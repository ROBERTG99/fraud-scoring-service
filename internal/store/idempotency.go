package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
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

// idemMagic prefixes every value: magic, 32-byte SHA-256 of the request, then the decision protobuf. A value
// without it is never served (M6 review, D-057): it could have been planted by anyone who reaches Redis, and a
// value written before the hash existed expires within IdempotencyTTL anyway.
var idemMagic = []byte("fidem1")

// delIfEqual deletes KEYS[1] only if it still holds ARGV[1] (a concurrent real Put must survive).
var delIfEqual = redis.NewScript(`if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) end return 0`)

// Get returns the stored decision and its request hash, or nils on a miss or an unprefixed value.
func (r *RedisIdempotency) Get(ctx context.Context, txID string) (*fraudv1.Decision, []byte, error) {
	b, err := r.rdb.Get(ctx, r.key(txID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if !bytes.HasPrefix(b, idemMagic) || len(b) < len(idemMagic)+sha256.Size {
		// Never served, and removed (only if unchanged since the read) so it cannot block the real decision's
		// SET NX for its TTL; best effort within the same deadline.
		_ = delIfEqual.Run(ctx, r.rdb, []string{r.key(txID)}, b).Err()
		return nil, nil, nil
	}
	hash := b[len(idemMagic) : len(idemMagic)+sha256.Size]
	b = b[len(idemMagic)+sha256.Size:]
	var d fraudv1.Decision
	if err := proto.Unmarshal(b, &d); err != nil {
		return nil, nil, err
	}
	return &d, hash, nil
}

// Put stores the decision and its request hash unless one is already stored (SET NX, 24 h TTL).
func (r *RedisIdempotency) Put(ctx context.Context, d *fraudv1.Decision, requestHash []byte) error {
	if len(requestHash) != sha256.Size {
		return fmt.Errorf("idempotency put: request hash has %d bytes, want %d", len(requestHash), sha256.Size)
	}
	b, err := proto.Marshal(d)
	if err != nil {
		return err
	}
	b = append(append(append([]byte{}, idemMagic...), requestHash...), b...)
	return r.rdb.SetNX(ctx, r.key(d.TransactionId), b, IdempotencyTTL).Err()
}
