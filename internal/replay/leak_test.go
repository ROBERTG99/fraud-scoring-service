//go:build integration

package replay

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/api"
	"github.com/ROBERTG99/fraud-scoring-service/internal/events"
	"github.com/ROBERTG99/fraud-scoring-service/internal/features"
	"github.com/ROBERTG99/fraud-scoring-service/internal/model"
	"github.com/ROBERTG99/fraud-scoring-service/internal/rules"
	"github.com/ROBERTG99/fraud-scoring-service/internal/store"
	"github.com/ROBERTG99/fraud-scoring-service/internal/testutil"
)

type lowModel struct{}

func (lowModel) Predict(_ context.Context, _ string, rows [][]float32) (model.Prediction, error) {
	return model.Prediction{Scores: make([]float32, len(rows)), Version: "test"}, nil
}

type memDecisions struct {
	mu sync.Mutex
	m  map[string]*fraudv1.Decision
}

func (d *memDecisions) Get(_ context.Context, id string) (*fraudv1.Decision, []byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.m[id], nil, nil
}
func (d *memDecisions) Put(context.Context, *fraudv1.Decision, []byte) error { return nil }
func (d *memDecisions) Commit(_ context.Context, r store.DecisionRecord) (*fraudv1.Decision, []byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.m[r.Decision.TransactionId] = r.Decision
	return r.Decision, r.RequestHash, nil
}
func (d *memDecisions) GetByTransaction(context.Context, string) (*fraudv1.Decision, []byte, error) {
	return nil, nil, store.ErrNotFound
}

// explaining scores with explain=true and keeps the feature vector per transaction.
type explaining struct {
	s     *api.ScorerServer
	mu    sync.Mutex
	feats map[string]map[string]float64
}

func (e *explaining) Score(ctx context.Context, in *fraudv1.ScoreRequest, _ ...grpc.CallOption) (*fraudv1.Decision, error) {
	in.Explain = true
	d, err := e.s.Score(ctx, in)
	if err == nil && d.Degraded {
		// Fallback defaults are not the store's features: report the timeout, never a "leak".
		return nil, fmt.Errorf("transaction %s scored in degraded mode %v (Redis slower than the 8 ms budget)", in.Transaction.TransactionId, d.ReasonCodes)
	}
	if err == nil {
		e.mu.Lock()
		e.feats[in.Transaction.TransactionId] = d.Features
		e.mu.Unlock()
	}
	return d, err
}

func leakFixture(t *testing.T) (*features.Store, *explaining, []events.Event, []events.Event) {
	t.Helper()
	spec, err := features.LoadSpec("../../features/spec.yaml")
	if err != nil {
		t.Fatal(err)
	}
	eng, err := rules.NewEngine("../../rules", nil)
	if err != nil {
		t.Fatal(err)
	}
	rdb := features.NewRedisClient(testutil.RedisAddr(t))
	t.Cleanup(func() { _ = rdb.Close() })
	fs := features.NewStore(rdb, testutil.UniqueName("leak"), spec)
	// Load the Lua scripts and open a connection first, so the first Score is not slowed past the 8 ms
	// Redis budget by EVALSHA falling back to EVAL on a cold connection.
	wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := fs.ReadAsOf(wctx, features.Tx{AccountID: "acc_warmup0", EventID: "warm", TSMillis: 1, DeviceID: "w", MCC: "5411", Channel: "ECOMMERCE"}); err != nil {
		t.Fatal(err)
	}
	if err := fs.ApplyEvent(wctx, &events.Event{Kind: events.KindLogin, EventID: "warm-login", AccountID: "acc_warmup0", TSMillis: 1, DeviceID: "w", Success: true}); err != nil {
		t.Fatal(err)
	}
	mem := &memDecisions{m: map[string]*fraudv1.Decision{}}
	// A replay of historical events: Score's 24 h event_ts bound is lifted, as in the seed stack (D-041).
	srv := api.NewScorerServer(api.ScorerDeps{Features: fs, Model: lowModel{}, Rules: eng, Idempotency: mem, Decisions: mem, MaxEventAge: -1})

	const acc = "acc_00000777"
	dev := func(c string) string { return c + strings.Repeat("0", 62) + c } // salted-hash shaped IDs
	devD, devE, devF := dev("d"), dev("e"), dev("f")
	t0 := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC).UnixMilli()
	login := func(id string, ts int64, dev string) events.Event {
		return events.Event{Kind: events.KindLogin, EventID: id, AccountID: acc, TSMillis: ts, DeviceID: dev, IPPrefix: "203.0.113.0/24",
			Country: "US", Success: true, Method: "password"}
	}
	tx := func(id string, ts int64, dev string) events.Event {
		e := txEvent(0, acc, ts, 4_000)
		e.EventID, e.DeviceID = id, dev
		return e
	}
	warm := []events.Event{
		login("login-d", t0-3*86_400_000, devD), // known device: first seen 3 days earlier
		login("login-f", t0-2*3_600_000, devF),  // another device, seen 2 h earlier
	}
	live := []events.Event{
		tx("0192f000-0000-7000-8000-0000000000a1", t0, devD),           // new_device=0, distinct_devices_24h=1 (F)
		login("login-f-later", t0+3_600_000, devF),                     // F again, after the transaction
		tx("0192f000-0000-7000-8000-0000000000a2", t0+7_200_000, devE), // a device never seen: new_device=1
	}
	return fs, &explaining{s: srv, feats: map[string]map[string]float64{}}, warm, live
}

func TestReplayDoesNotLeakLaterEventsIntoFeatures(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fs, sc, warm, live := leakFixture(t)
	for i := range warm { // what the feature-updater does with the warm phase
		if err := fs.ApplyEvent(ctx, &warm[i]); err != nil {
			t.Fatal(err)
		}
	}
	st, err := Run(ctx, Config{Workers: 4, Applier: fs}, nil, sc, nil, live)
	if err != nil || st.Scored != 2 || st.Applied != 1 {
		t.Fatalf("run %+v %v", st, err)
	}
	first := sc.feats[live[0].EventID]
	if first["new_device"] != 0 || first["distinct_devices_24h"] != 1 {
		t.Fatalf("transaction on a known device: new_device=%v distinct_devices_24h=%v, want 0 and 1 (a later sighting leaked in?)",
			first["new_device"], first["distinct_devices_24h"])
	}
	if got := sc.feats[live[2].EventID]["new_device"]; got != 1 {
		t.Fatalf("unseen device scored new_device=%v", got)
	}
}

// The order the first replay used (all live account events first) shows the leak this test guards: the
// later sighting of F moves its last-seen time past the transaction (ZADD GT), so F drops out of
// distinct_devices_24h. new_device (feature spec v2, first sighting, ZADD LT) is immune to the order.
func TestLoadingTheWindowFirstLeaks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fs, sc, warm, live := leakFixture(t)
	for _, e := range append(append([]events.Event{}, warm...), live[1]) {
		if err := fs.ApplyEvent(ctx, &e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sc.Score(ctx, &fraudv1.ScoreRequest{Transaction: Transaction(&live[0])}); err != nil {
		t.Fatal(err)
	}
	got := sc.feats[live[0].EventID]
	if got["distinct_devices_24h"] != 0 {
		t.Fatalf("expected the ZADD GT overwrite to hide F (distinct_devices_24h=0), got %v", got["distinct_devices_24h"])
	}
	if got["new_device"] != 0 {
		t.Fatalf("new_device must not depend on arrival order, got %v", got["new_device"])
	}
}
