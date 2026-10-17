//go:build integration

package features

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"pgregory.net/rapid"

	"github.com/ROBERTG99/fraud-scoring-service/internal/events"
	"github.com/ROBERTG99/fraud-scoring-service/internal/testutil"
)

type countingHook struct{ n atomic.Int64 }

func (h *countingHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *countingHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		h.n.Add(1)
		return next(ctx, cmd)
	}
}
func (h *countingHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func newTestStore(t *testing.T) (*Store, *redis.Client) {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: testutil.RedisAddr(t)})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewStore(rdb, testutil.UniqueName("t"), nil), rdb
}

const base = int64(1_788_000_000_000)

func TestReadAsOfThenAddIsAtomic(t *testing.T) {
	s, rdb := newTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Warm the script cache so the counted call is the steady-state path.
	if _, _, err := s.ScoreTx(ctx, Tx{EventID: "warm", AccountID: "acc_warm", TSMillis: base, AmountMinor: 1, DeviceID: "d", MCC: "5411", Channel: "ECOMMERCE"}); err != nil {
		t.Fatal(err)
	}
	hook := &countingHook{}
	rdb.AddHook(hook)

	tx := Tx{EventID: "e1", AccountID: "acc_1", TSMillis: base, AmountMinor: 500, DeviceID: "dev1", MCC: "5411", Channel: "ECOMMERCE"}
	v, _, err := s.ScoreTx(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	if got := hook.n.Load(); got != 1 {
		t.Fatalf("ScoreTx used %d round trips; read and add must be one atomic script call", got)
	}
	if v[IdxTxCount24h] != 0 || v[IdxNewDevice] != 1 {
		t.Fatalf("features must exclude the transaction being scored: %v", v)
	}
	if n, _ := rdb.ZCard(ctx, s.Keys("acc_1")[0]).Result(); n != 1 {
		t.Fatalf("transaction not added (zcard=%d)", n)
	}
	// Replaying the same transaction (idempotent retry) reads the same features and adds nothing.
	v2, _, err := s.ScoreTx(ctx, tx)
	if err != nil || v2 != v {
		t.Fatalf("replay: %v %v", v2, err)
	}
	if n, _ := rdb.ZCard(ctx, s.Keys("acc_1")[0]).Result(); n != 1 {
		t.Fatalf("replay added a duplicate (zcard=%d)", n)
	}

	// Concurrent scoring on one account: every call is serialised by Redis, so each later
	// transaction observes a subset of earlier ones and the final state holds all of them.
	const n = 64
	var wg sync.WaitGroup
	seen := make([]float64, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, _, err := s.ScoreTx(ctx, Tx{EventID: fmt.Sprintf("c%d", i), AccountID: "acc_2", TSMillis: base + int64(i)*1000, AmountMinor: 100, DeviceID: "d", MCC: "5411", Channel: "ECOMMERCE"})
			if err != nil {
				t.Error(err)
				return
			}
			seen[i] = v[IdxTxCount10m]
		}(i)
	}
	wg.Wait()
	for i, c := range seen {
		if c > float64(i) {
			t.Fatalf("tx %d saw %v earlier transactions, at most %d exist", i, c, i)
		}
	}
	if n2, _ := rdb.ZCard(ctx, s.Keys("acc_2")[0]).Result(); n2 != n {
		t.Fatalf("zcard=%d want %d", n2, n)
	}
}

type genEv struct {
	ev      events.Event
	arrival int64
}

// TestOutOfOrderWithinFiveMinutes: events arrive up to 5 minutes late; reading as of a transaction
// timestamp only ever counts events strictly earlier than it.
func TestOutOfOrderWithinFiveMinutes(t *testing.T) {
	s, _ := newTestStore(t)
	devs := []string{"d1", "d2", "d3"}
	mccs := []string{"5411", "5732", "4829"}
	changes := []string{events.ChangeDeviceBound, events.ChangePasswordChanged, events.ChangeEmailChanged}
	var caseN atomic.Int64
	rapid.Check(t, func(rt *rapid.T) {
		acc := fmt.Sprintf("acc_%d", caseN.Add(1))
		n := rapid.IntRange(2, 40).Draw(rt, "n")
		used := map[int64]bool{}
		var evs []genEv
		for i := 0; i < n; i++ {
			ts := base + rapid.Int64Range(0, 3*Ms24h).Draw(rt, "ts") // crosses the 24 h new_device boundary
			if used[ts] {
				continue
			}
			used[ts] = true
			e := events.Event{AccountID: acc, EventID: fmt.Sprintf("%s-e%d", acc, i), TSMillis: ts, DeviceID: rapid.SampledFrom(devs).Draw(rt, "dev")}
			switch rapid.IntRange(0, 4).Draw(rt, "kind") {
			case 0:
				e.Kind, e.Success = events.KindLogin, rapid.Bool().Draw(rt, "ok")
			case 1:
				e.Kind, e.Change = events.KindDevice, rapid.SampledFrom(changes).Draw(rt, "change")
			default:
				e.Kind, e.AmountMinor, e.MCC, e.Channel = events.KindTransaction, rapid.Int64Range(1, 100_000).Draw(rt, "amt"), rapid.SampledFrom(mccs).Draw(rt, "mcc"), "ECOMMERCE"
			}
			delay := rapid.Int64Range(0, 5*60_000).Draw(rt, "delay")
			evs = append(evs, genEv{ev: e, arrival: ts + delay})
		}
		sort.SliceStable(evs, func(i, j int) bool { return evs[i].arrival < evs[j].arrival })
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var arrived []events.Event
		for _, g := range evs {
			e := g.ev
			if e.Kind != events.KindTransaction {
				if err := s.ApplyEvent(ctx, &e); err != nil {
					rt.Fatal(err)
				}
				arrived = append(arrived, e)
				continue
			}
			v, _, err := s.ScoreTx(ctx, TxFromEvent(&e))
			if err != nil {
				rt.Fatal(err)
			}
			checkOnlyEarlier(rt, v, e, arrived)
			arrived = append(arrived, e)
		}
	})
}

func checkOnlyEarlier(rt *rapid.T, v Vector, tx events.Event, arrived []events.Event) {
	T := tx.TSMillis
	var c10, c1h, c24, sum1h int64
	devSeen := map[string]int64{}
	devFirst := map[string]int64{}
	pwd, email := int64(-1), int64(-1)
	for _, a := range arrived {
		if a.TSMillis >= T {
			continue
		}
		if a.Kind == events.KindTransaction {
			if a.TSMillis >= T-Ms10m {
				c10++
			}
			if a.TSMillis >= T-Ms1h {
				c1h++
				sum1h += a.AmountMinor
			}
			if a.TSMillis >= T-Ms24h {
				c24++
			}
		}
		if a.Kind == events.KindTransaction || (a.Kind == events.KindLogin && a.Success) || (a.Kind == events.KindDevice && a.Change == events.ChangeDeviceBound) {
			devSeen[a.DeviceID] = max(devSeen[a.DeviceID], a.TSMillis)
			if f, ok := devFirst[a.DeviceID]; !ok || a.TSMillis < f {
				devFirst[a.DeviceID] = a.TSMillis
			}
		}
		if a.Kind == events.KindDevice && a.Change == events.ChangePasswordChanged {
			pwd = max(pwd, a.TSMillis)
		}
		if a.Kind == events.KindDevice && a.Change == events.ChangeEmailChanged {
			email = max(email, a.TSMillis)
		}
	}
	// Transaction windows are exact over arrived earlier events: later or unarrived events never count.
	if v[IdxTxCount10m] != float64(c10) || v[IdxTxCount1h] != float64(c1h) || v[IdxTxCount24h] != float64(c24) || v[IdxAmountSum1h] != float64(sum1h) {
		rt.Fatalf("tx windows %v want c10=%d c1h=%d c24=%d sum1h=%d", v, c10, c1h, c24, sum1h)
	}
	// Device and MCC state keep only the latest sighting (ZADD GT, spec §4.4), so they may under-count
	// when a later event arrived first, but never count a later event (docs/KNOWN_LIMITATIONS.md).
	if v[IdxDistinctDevices24h] > float64(len(devSeen)) {
		rt.Fatalf("distinct devices %v > %d earlier devices", v[IdxDistinctDevices24h], len(devSeen))
	}
	// new_device (spec v2) reads the first sighting (ZADD LT), which later arrivals can never lower, so it
	// is exact under any arrival order: 0 iff an arrived sighting of the device precedes T - 24 h.
	wantNew := 1.0
	if first, ok := devFirst[tx.DeviceID]; ok && first < T-Ms24h {
		wantNew = 0
	}
	if v[IdxNewDevice] != wantNew {
		rt.Fatalf("device %s first seen %d (T=%d): new_device=%v, want %v", tx.DeviceID, devFirst[tx.DeviceID], T, v[IdxNewDevice], wantNew)
	}
	// Credential changes keep a bounded history, so minutes_since is exact.
	for _, c := range []struct {
		got  float64
		last int64
	}{{v[IdxMinutesSincePwd], pwd}, {v[IdxMinutesSinceEmail], email}} {
		exact := float64(MinutesCap)
		if c.last >= 0 {
			exact = float64(min((T-c.last)/60_000, MinutesCap))
		}
		if c.got != exact {
			rt.Fatalf("minutes_since %v, want %v", c.got, exact)
		}
	}
}

func TestApplyEventsPipelinedMatchesSingle(t *testing.T) {
	s1, _ := newTestStore(t)
	s2, _ := newTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var evs []events.Event
	for i := 0; i < 1200; i++ { // more than MaxPipeline, so several chunks
		e := events.Event{EventID: fmt.Sprintf("e%d", i), AccountID: fmt.Sprintf("acc_%d", i%7), TSMillis: base + int64(i)*1000, DeviceID: fmt.Sprintf("d%d", i%5)}
		switch i % 4 {
		case 0:
			e.Kind, e.Success = events.KindLogin, true
		case 1:
			e.Kind, e.Change = events.KindDevice, []string{events.ChangeDeviceBound, events.ChangePasswordChanged, events.ChangeEmailChanged}[i%3]
		default:
			e.Kind, e.AmountMinor, e.MCC, e.Channel = events.KindTransaction, int64(100+i), "5411", "ECOMMERCE"
		}
		evs = append(evs, e)
	}
	if err := s1.ApplyEvents(ctx, evs); err != nil {
		t.Fatal(err)
	}
	for i := range evs {
		if err := s2.ApplyEvent(ctx, &evs[i]); err != nil {
			t.Fatal(err)
		}
	}
	for a := 0; a < 7; a++ {
		tx := Tx{EventID: "probe", AccountID: fmt.Sprintf("acc_%d", a), TSMillis: base + 1201*1000, AmountMinor: 500, DeviceID: "d1", MCC: "5411", Channel: "ECOMMERCE"}
		v1, _, err1 := s1.ReadAsOf(ctx, tx)
		v2, _, err2 := s2.ReadAsOf(ctx, tx)
		if err1 != nil || err2 != nil || v1 != v2 {
			t.Fatalf("account %d: pipelined %v vs single %v (%v %v)", a, v1, v2, err1, err2)
		}
	}
	if err := s1.ApplyEvents(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if v := s1.Fallback(Tx{AmountMinor: 7, Channel: "P2P"}); v[IdxAmountMinor] != 7 {
		t.Fatal("fallback")
	}
	if _, err := LoadSpec(""); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSpec("/nonexistent/spec.yaml"); err == nil {
		t.Fatal("missing spec must fail")
	}
}

func TestScoreTxUnavailable(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	defer func() { _ = rdb.Close() }()
	s := NewStore(rdb, "x", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, _, err := s.ScoreTx(ctx, Tx{AccountID: "a", TSMillis: base}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
	if err := s.ApplyEvent(ctx, &events.Event{Kind: events.KindLogin, AccountID: "a"}); err == nil {
		t.Fatal("apply against a dead server must fail")
	}
}
