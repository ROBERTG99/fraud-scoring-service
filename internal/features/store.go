package features

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ROBERTG99/fraud-scoring-service/internal/events"
)

//go:embed score_tx.lua
var scoreTxLua string

//go:embed apply_event.lua
var applyEventLua string

var (
	scoreTxScript    = redis.NewScript(scoreTxLua)
	applyEventScript = redis.NewScript(applyEventLua)
)

// ErrUnavailable means features could not be read in time; callers fall back to defaults.
var ErrUnavailable = errors.New("features unavailable")

// Tx is the subset of a transaction the feature store needs.
type Tx struct {
	EventID     string
	AccountID   string
	TSMillis    int64
	AmountMinor int64
	DeviceID    string
	MCC         string
	Channel     string
}

// Raw holds the values read by score_tx.lua (-1 means "absent").
type Raw struct {
	Count10m, Count1h, Sum1h, Count24h, Sum24h, Distinct24h int64
	DeviceFirst, MCCSeen, PwdChanged, EmailChanged          int64 // DeviceFirst: first sighting of the tx device, -1 if none
}

// Store reads and updates per-account feature state in Redis.
type Store struct {
	rdb    redis.Scripter
	prefix string
	spec   *Spec
}

// NewStore returns a store. prefix defaults to "fr" (keys fr:{acc}:tx, ...); tests use unique prefixes.
func NewStore(rdb redis.Scripter, prefix string, spec *Spec) *Store {
	if prefix == "" {
		prefix = "fr"
	}
	if spec == nil {
		spec = DefaultSpec()
	}
	return &Store{rdb: rdb, prefix: prefix, spec: spec}
}

// NewRedisClient builds a client tuned for the scoring path (pool 256, short timeouts). Socket
// timeouts are disabled so the caller's context deadline (8 ms on Score) governs every call.
func NewRedisClient(addr string) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:                  addr,
		PoolSize:              256,
		MinIdleConns:          32,
		DialTimeout:           200 * time.Millisecond,
		ReadTimeout:           -1,
		WriteTimeout:          -1,
		PoolTimeout:           20 * time.Millisecond,
		MaxRetries:            -1,
		ContextTimeoutEnabled: true,
	})
}

// NewUpdaterRedisClient builds a client for the feature-updater: batches of pipelined scripts can take
// longer than a single Score call, so only the per-batch context deadline (2 s) bounds them.
func NewUpdaterRedisClient(addr string) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:                  addr,
		PoolSize:              32,
		DialTimeout:           time.Second,
		ReadTimeout:           -1,
		WriteTimeout:          -1,
		PoolTimeout:           time.Second,
		ContextTimeoutEnabled: true,
	})
}

// MaxPipeline bounds one pipelined round trip in ApplyEvents.
const MaxPipeline = 500

// Keys returns the five per-account keys; the {account} hash tag keeps them in one cluster slot.
func (s *Store) Keys(account string) []string {
	base := s.prefix + ":{" + account + "}:"
	return []string{base + "tx", base + "dev", base + "mcc", base + "cred", base + "devfirst"}
}

// Spec returns the store's feature spec.
func (s *Store) Spec() *Spec { return s.spec }

// ScoreTx reads features as of tx.TSMillis (strictly earlier events) and adds tx, atomically.
func (s *Store) ScoreTx(ctx context.Context, tx Tx) (Vector, Raw, error) {
	raw, err := s.run(ctx, tx, true)
	if err != nil {
		return Vector{}, Raw{}, err
	}
	return s.Compute(raw, tx), raw, nil
}

// ReadAsOf reads features as of tx.TSMillis without adding the transaction.
func (s *Store) ReadAsOf(ctx context.Context, tx Tx) (Vector, Raw, error) {
	raw, err := s.run(ctx, tx, false)
	if err != nil {
		return Vector{}, Raw{}, err
	}
	return s.Compute(raw, tx), raw, nil
}

func (s *Store) run(ctx context.Context, tx Tx, add bool) (Raw, error) {
	addFlag := "0"
	if add {
		addFlag = "1"
	}
	res, err := scoreTxScript.Run(ctx, s.rdb, s.Keys(tx.AccountID),
		tx.TSMillis, tx.EventID, tx.AmountMinor, tx.DeviceID, tx.MCC, addFlag).Int64Slice()
	if err != nil {
		return Raw{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if len(res) != 10 {
		return Raw{}, fmt.Errorf("%w: score_tx returned %d values", ErrUnavailable, len(res))
	}
	return Raw{Count10m: res[0], Count1h: res[1], Sum1h: res[2], Count24h: res[3], Sum24h: res[4], Distinct24h: res[5],
		DeviceFirst: res[6], MCCSeen: res[7], PwdChanged: res[8], EmailChanged: res[9]}, nil
}

// Compute turns raw Redis values into the feature vector (must match training/features.py exactly).
func (s *Store) Compute(r Raw, tx Tx) Vector {
	t := tx.TSMillis
	var v Vector
	v[IdxTxCount10m] = float64(r.Count10m)
	v[IdxTxCount1h] = float64(r.Count1h)
	v[IdxTxCount24h] = float64(r.Count24h)
	v[IdxAmountSum1h] = float64(r.Sum1h)
	v[IdxAmountMinor] = float64(tx.AmountMinor)
	v[IdxDistinctDevices24h] = float64(r.Distinct24h)
	// Feature spec v2: a device is new until 24 h after its first sighting on the account.
	v[IdxNewDevice] = 1
	if r.DeviceFirst >= 0 && r.DeviceFirst < t-Ms24h {
		v[IdxNewDevice] = 0
	}
	v[IdxMinutesSincePwd] = minutesSince(r.PwdChanged, t)
	v[IdxMinutesSinceEmail] = minutesSince(r.EmailChanged, t)
	v[IdxMCCNovel30d] = 1
	if r.MCCSeen >= 0 && r.MCCSeen < t && r.MCCSeen >= t-Ms30d {
		v[IdxMCCNovel30d] = 0
	}
	v[IdxChannel] = s.spec.ChannelCode(tx.Channel)
	v[IdxHourOfDay] = float64((t / Ms1h) % 24)
	v[IdxAmountRatio24h] = 1
	if r.Count24h > 0 {
		v[IdxAmountRatio24h] = float64(tx.AmountMinor) / (float64(r.Sum24h) / float64(r.Count24h))
	}
	return v
}

func minutesSince(changed, t int64) float64 {
	if changed < 0 || changed >= t {
		return MinutesCap
	}
	return float64(min((t-changed)/60_000, MinutesCap))
}

// ApplyEvent applies one event to the account's feature state.
func (s *Store) ApplyEvent(ctx context.Context, e *events.Event) error {
	if err := applyEventScript.Run(ctx, s.rdb, s.Keys(e.AccountID), applyArgs(e)...).Err(); err != nil {
		return fmt.Errorf("apply event %s: %w", e.EventID, err)
	}
	return nil
}

type pipeliner interface {
	Pipeline() redis.Pipeliner
}

// ApplyEvents applies a batch of events in one pipelined round trip (feature-updater hot path).
func (s *Store) ApplyEvents(ctx context.Context, evs []events.Event) error {
	if len(evs) == 0 {
		return nil
	}
	pl, ok := s.rdb.(pipeliner)
	if !ok {
		for i := range evs {
			if err := s.ApplyEvent(ctx, &evs[i]); err != nil {
				return err
			}
		}
		return nil
	}
	for start := 0; start < len(evs); start += MaxPipeline {
		if err := s.applyChunk(ctx, pl, evs[start:min(start+MaxPipeline, len(evs))]); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) applyChunk(ctx context.Context, pl pipeliner, evs []events.Event) error {
	for attempt := 0; attempt < 2; attempt++ {
		pipe := pl.Pipeline()
		for i := range evs {
			e := &evs[i]
			applyEventScript.EvalSha(ctx, pipe, s.Keys(e.AccountID), applyArgs(e)...)
		}
		_, err := pipe.Exec(ctx)
		if err == nil {
			return nil
		}
		if attempt == 0 && redis.HasErrorPrefix(err, "NOSCRIPT") {
			if err := applyEventScript.Load(ctx, s.rdb).Err(); err != nil {
				return err
			}
			continue
		}
		return fmt.Errorf("apply %d events: %w", len(evs), err)
	}
	return nil
}

func applyArgs(e *events.Event) []any {
	kind := "tx"
	switch e.Kind {
	case events.KindLogin:
		kind = "login"
	case events.KindDevice:
		kind = "device"
	}
	success := "0"
	if e.Success {
		success = "1"
	}
	return []any{kind, strconv.FormatInt(e.TSMillis, 10), e.EventID, e.AmountMinor, e.DeviceID, e.MCC, e.Change, success}
}

// TxFromEvent converts a transaction event to a feature-store Tx.
func TxFromEvent(e *events.Event) Tx {
	return Tx{EventID: e.EventID, AccountID: e.AccountID, TSMillis: e.TSMillis, AmountMinor: e.AmountMinor,
		DeviceID: e.DeviceID, MCC: e.MCC, Channel: e.Channel}
}
