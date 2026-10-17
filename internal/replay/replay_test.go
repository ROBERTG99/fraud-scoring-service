package replay

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/events"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return nil
}

type recordingScorer struct {
	mu     sync.Mutex
	order  map[string][]int64
	tokens map[string]int
	fail   map[string]error
}

func (s *recordingScorer) Score(ctx context.Context, in *fraudv1.ScoreRequest, _ ...grpc.CallOption) (*fraudv1.Decision, error) {
	md, _ := metadata.FromOutgoingContext(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.order == nil {
		s.order, s.tokens = map[string][]int64{}, map[string]int{}
	}
	tx := in.Transaction
	s.order[tx.AccountId] = append(s.order[tx.AccountId], tx.EventTs.AsTime().UnixMilli())
	for _, v := range md.Get("authorization") {
		s.tokens[v]++
	}
	if err := s.fail[tx.TransactionId]; err != nil {
		return nil, err
	}
	action := fraudv1.Action_ACTION_APPROVE
	if tx.AmountMinor >= 50_000 {
		action = fraudv1.Action_ACTION_REVIEW
	}
	return &fraudv1.Decision{TransactionId: tx.TransactionId, Action: action}, nil
}

type recordingProducer struct {
	mu      sync.Mutex
	records []*kgo.Record
	err     error
}

func (p *recordingProducer) ProduceSync(_ context.Context, rs ...*kgo.Record) kgo.ProduceResults {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(kgo.ProduceResults, 0, len(rs))
	for _, r := range rs {
		if p.err == nil {
			p.records = append(p.records, r)
		}
		out = append(out, kgo.ProduceResult{Record: r, Err: p.err})
	}
	return out
}

func txEvent(i int, account string, ts int64, amount int64) events.Event {
	return events.Event{Kind: events.KindTransaction, EventID: fmt.Sprintf("0192f000-0000-7000-8000-%012d", i), AccountID: account,
		TSMillis: ts, DeviceID: "ab", IPPrefix: "203.0.113.0/24", Country: "US", AmountMinor: amount, Currency: "USD",
		MerchantID: "m_00042", MCC: "5732", Channel: events.ChannelEcommerce}
}

func dataset(n int) []events.Event {
	base := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC).UnixMilli()
	var out []events.Event
	for i := range n {
		acc := fmt.Sprintf("acc_%08d", i%7)
		ts := base + int64(i)*60_000
		if i%5 == 0 {
			out = append(out, events.Event{Kind: events.KindDevice, EventID: fmt.Sprintf("dev-%d", i), AccountID: acc, TSMillis: ts - 1,
				DeviceID: "ab", Change: events.ChangeEmailChanged})
		}
		out = append(out, txEvent(i, acc, ts, int64(1000+i*1000)))
	}
	return out
}

func TestReplayRateLimit(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	p := NewPacer(100, clock)
	for range 50 {
		if err := p.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := clock.Now().Sub(time.Unix(0, 0)); got != 490*time.Millisecond {
		t.Fatalf("50 events at 100/s took %v of clock time, want 490ms (first event immediate)", got)
	}

	clock = &fakeClock{now: time.Unix(0, 0)}
	sc := &recordingScorer{}
	evs := dataset(100)
	st, err := Run(context.Background(), Config{Rate: 200, Workers: 4, Clock: clock, Applier: &recordingApplier{sc: &recordingScorer{}}}, nil, sc, nil, evs)
	if err != nil {
		t.Fatal(err)
	}
	if st.Scored != 100 || st.Errors != 0 {
		t.Fatalf("stats %+v", st)
	}
	if got := clock.Now().Sub(time.Unix(0, 0)); got != 495*time.Millisecond {
		t.Fatalf("100 transactions at 200/s took %v of clock time, want 495ms", got)
	}
	// Unlimited rate never sleeps.
	clock = &fakeClock{now: time.Unix(0, 0)}
	if _, err := Run(context.Background(), Config{Clock: clock, Applier: &recordingApplier{sc: &recordingScorer{}}}, nil, &recordingScorer{}, nil, evs); err != nil || !clock.Now().Equal(time.Unix(0, 0)) {
		t.Fatalf("unlimited: %v %v", err, clock.Now())
	}
}

func TestPacerCatchesUpWithoutBurst(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	p := NewPacer(10, clock)
	_ = p.Wait(context.Background())
	clock.now = clock.now.Add(5 * time.Second) // a slow consumer stalls the dispatcher
	_ = p.Wait(context.Background())
	before := clock.Now()
	_ = p.Wait(context.Background())
	if clock.Now().Sub(before) != 100*time.Millisecond {
		t.Fatal("a stall must not turn into a burst")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}

type recordingApplier struct {
	mu   sync.Mutex
	sc   *recordingScorer
	seen []string
	fail string
}

func (a *recordingApplier) ApplyEvent(_ context.Context, e *events.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e.EventID == a.fail {
		return errors.New("redis down")
	}
	a.seen = append(a.seen, e.EventID)
	// Interleave with the scorer's log so per-account order covers both kinds.
	a.sc.mu.Lock()
	if a.sc.order == nil {
		a.sc.order, a.sc.tokens = map[string][]int64{}, map[string]int{}
	}
	a.sc.order[e.AccountID] = append(a.sc.order[e.AccountID], e.TSMillis)
	a.sc.mu.Unlock()
	return nil
}

func TestPhasesOrderAndPerAccountOrdering(t *testing.T) {
	evs := dataset(200)
	warm, live := Split(evs, 2*time.Hour)
	if len(warm) == 0 || len(live) == 0 || warm[len(warm)-1].TSMillis > live[0].TSMillis {
		t.Fatalf("split %d/%d", len(warm), len(live))
	}
	prod := &recordingProducer{}
	sc := &recordingScorer{}
	app := &recordingApplier{sc: sc}
	var producedAtWait int
	cfg := Config{Workers: 8, Token: "svc", Applier: app, WaitCaughtUp: func(context.Context) error {
		producedAtWait = len(prod.records)
		if len(sc.order) != 0 {
			t.Error("live work started before the feature-updater caught up on the warm events")
		}
		return nil
	}}
	st, err := Run(context.Background(), cfg, prod, sc, warm, live)
	if err != nil {
		t.Fatal(err)
	}
	liveTx, liveOther := 0, 0
	for _, e := range live {
		if e.Kind == events.KindTransaction {
			liveTx++
		} else {
			liveOther++
		}
	}
	if producedAtWait != len(warm) || st.Produced != len(warm)+liveOther || st.Applied != liveOther || st.Scored != liveTx {
		t.Fatalf("stats %+v (warm %d at wait %d, live other %d, live tx %d)", st, len(warm), producedAtWait, liveOther, liveTx)
	}
	for _, r := range prod.records {
		if string(r.Key) == "" || r.Topic == "" {
			t.Fatal("records need topic and account key")
		}
	}
	for acc, ts := range sc.order {
		for i := 1; i < len(ts); i++ {
			if ts[i] < ts[i-1] {
				t.Fatalf("%s handled out of event order: %v", acc, ts)
			}
		}
	}
	if sc.tokens["Bearer svc"] != liveTx {
		t.Fatalf("token sent on %d of %d calls", sc.tokens["Bearer svc"], liveTx)
	}
	if st.Actions["REVIEW"]+st.Actions["APPROVE"] != liveTx {
		t.Fatalf("actions %v", st.Actions)
	}
}

func TestLiveAccountEventsNeedAnApplier(t *testing.T) {
	evs := dataset(20)
	if _, err := Run(context.Background(), Config{}, nil, &recordingScorer{}, nil, evs); err == nil {
		t.Fatal("login/device events without an Applier must be refused, not silently dropped")
	}
	sc := &recordingScorer{}
	app := &recordingApplier{sc: sc, fail: "dev-5"}
	st, err := Run(context.Background(), Config{Applier: app}, nil, sc, nil, evs)
	if err != nil || st.ApplyErrors != 1 || st.Applied != 3 {
		t.Fatalf("apply errors are counted: %+v %v", st, err)
	}
}

func TestScoreErrorsAreCountedNotFatal(t *testing.T) {
	evs := dataset(10)
	sc := &recordingScorer{fail: map[string]error{evs[2].EventID: status.Error(codes.ResourceExhausted, "queue full")}}
	st, err := Run(context.Background(), Config{Applier: &recordingApplier{sc: &recordingScorer{}}}, nil, sc, nil, evs)
	if err != nil || st.Errors != 1 || st.ByCode["ResourceExhausted"] != 1 {
		t.Fatalf("stats %+v err %v", st, err)
	}
	if _, err := Run(context.Background(), Config{}, &recordingProducer{err: errors.New("broker down")}, sc, evs, nil); err == nil {
		t.Fatal("produce failure must fail the run")
	}
	waitErr := errors.New("timeout")
	if _, err := Run(context.Background(), Config{Applier: &recordingApplier{sc: &recordingScorer{}}, WaitCaughtUp: func(context.Context) error { return waitErr }}, nil, sc, nil, evs); !errors.Is(err, waitErr) {
		t.Fatalf("wait failure: %v", err)
	}
}

func TestLoadSortsAndRejectsBadLines(t *testing.T) {
	dir := t.TempDir()
	evs := dataset(20)
	var buf []byte
	for i := len(evs) - 1; i >= 0; i-- { // reverse order on disk
		buf = evs[i].AppendJSON(buf)
		buf = append(buf, '\n')
	}
	path := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(path, append(buf, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil || len(got) != len(evs) {
		t.Fatalf("load %d %v", len(got), err)
	}
	for i := 1; i < len(got); i++ {
		if got[i].TSMillis < got[i-1].TSMillis {
			t.Fatal("not sorted")
		}
	}
	tx := Transaction(&got[len(got)-1])
	if tx.Channel != fraudv1.Channel_CHANNEL_ECOMMERCE || tx.EventTs.AsTime().UnixMilli() != got[len(got)-1].TSMillis {
		t.Fatalf("transaction %+v", tx)
	}
	if err := os.WriteFile(path, []byte("{\"schema\":\"nope\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("bad line must fail")
	}
	if _, err := Load(filepath.Join(dir, "missing.jsonl")); err == nil {
		t.Fatal("missing file must fail")
	}
	if w, l := Split(nil, time.Hour); w != nil || l != nil {
		t.Fatal("empty split")
	}
}

func TestGroupLagAndCaughtUp(t *testing.T) {
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(2, "events.login", "events.device"))
	if err != nil {
		t.Fatal(err)
	}
	defer cluster.Close()
	cl, err := kgo.NewClient(kgo.SeedBrokers(cluster.ListenAddrs()...))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	adm := kadm.NewClient(cl)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	evs := dataset(0)
	for i := range 10 {
		evs = append(evs, events.Event{Kind: events.KindLogin, EventID: fmt.Sprintf("l-%d", i), AccountID: fmt.Sprintf("acc_%08d", i),
			TSMillis: int64(i), DeviceID: "ab", IPPrefix: "203.0.113.0/24", Country: "US", Success: true, Method: "password"})
	}
	if _, err := produce(ctx, cl, evs, func(*events.Event) bool { return true }); err != nil {
		t.Fatal(err)
	}
	topics := []string{"events.login", "events.device"}
	lag, err := GroupLag(ctx, adm, "feature-updater", topics)
	if err != nil || lag != 10 {
		t.Fatalf("lag before consuming = %d, %v; want 10", lag, err)
	}
	short, cancelShort := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancelShort()
	if err := CaughtUp(adm, "feature-updater", topics, 20*time.Millisecond)(short); err == nil {
		t.Fatal("CaughtUp must time out while the group is behind")
	}

	consumer, err := kgo.NewClient(kgo.SeedBrokers(cluster.ListenAddrs()...), kgo.ConsumerGroup("feature-updater"),
		kgo.ConsumeTopics(topics...), kgo.DisableAutoCommit())
	if err != nil {
		t.Fatal(err)
	}
	defer consumer.Close()
	seen := 0
	for seen < 10 {
		fs := consumer.PollFetches(ctx)
		if err := ctx.Err(); err != nil {
			t.Fatal(err)
		}
		seen += fs.NumRecords()
	}
	if err := consumer.CommitUncommittedOffsets(ctx); err != nil {
		t.Fatal(err)
	}
	if err := CaughtUp(adm, "feature-updater", topics, 20*time.Millisecond)(ctx); err != nil {
		t.Fatal(err)
	}
}
