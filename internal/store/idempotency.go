package store

import (
	"bytes"
	"context"
	"crypto/hmac"
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

// RedisIdempotency stores serialized decisions at fr:idem:score:{transaction_id} with SET NX. Each value is
// MACed under a server key, so whoever can write Redis cannot forge or edit one (D-062).
type RedisIdempotency struct {
	rdb    redis.Cmdable
	prefix string
	key    []byte
}

// NewRedisIdempotency returns the store; prefix defaults to "fr", macKey authenticates the values.
func NewRedisIdempotency(rdb redis.Cmdable, prefix string, macKey []byte) *RedisIdempotency {
	if prefix == "" {
		prefix = "fr"
	}
	return &RedisIdempotency{rdb: rdb, prefix: prefix, key: macKey}
}

func (r *RedisIdempotency) redisKey(txID string) string { return r.prefix + ":idem:score:" + txID }

// idemMagic prefixes every value: magic, the 32-byte SHA-256 of the request, a 32-byte HMAC-SHA256 under the
// server key over the transaction_id, the request hash and the decision bytes, then the decision protobuf. A
// value without it, or whose MAC does not verify, is never served and is removed (D-057, D-062).
var idemMagic = []byte("fidem2")

const idemHeader = 6 + sha256.Size + sha256.Size

func (r *RedisIdempotency) mac(txID string, requestHash, decision []byte) []byte {
	m := hmac.New(sha256.New, r.key)
	m.Write([]byte("fraud.idem.v2\x00" + txID + "\x00"))
	m.Write(requestHash)
	m.Write(decision)
	return m.Sum(nil)
}

// delIfEqual deletes KEYS[1] only if it still holds ARGV[1] (a concurrent real Put must survive).
var delIfEqual = redis.NewScript(`if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) end return 0`)

// Get returns the stored decision and its request hash, or nils on a miss or a value that does not verify.
func (r *RedisIdempotency) Get(ctx context.Context, txID string) (*fraudv1.Decision, []byte, error) {
	b, err := r.rdb.Get(ctx, r.redisKey(txID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if !bytes.HasPrefix(b, idemMagic) || len(b) < idemHeader ||
		!hmac.Equal(b[6+sha256.Size:idemHeader], r.mac(txID, b[6:6+sha256.Size], b[idemHeader:])) {
		// Never served, and removed (only if unchanged since the read) so it cannot block the real decision's
		// SET NX for its TTL; best effort within the same deadline.
		_ = delIfEqual.Run(ctx, r.rdb, []string{r.redisKey(txID)}, b).Err()
		return nil, nil, nil
	}
	hash := b[6 : 6+sha256.Size]
	var d fraudv1.Decision
	if err := proto.Unmarshal(b[idemHeader:], &d); err != nil {
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
	v := make([]byte, 0, idemHeader+len(b))
	v = append(append(append(append(v, idemMagic...), requestHash...), r.mac(d.TransactionId, requestHash, b)...), b...)
	return r.rdb.SetNX(ctx, r.redisKey(d.TransactionId), v, IdempotencyTTL).Err()
}
