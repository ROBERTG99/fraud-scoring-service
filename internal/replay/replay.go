// Package replay feeds a generated dataset through the running system.
//
// Point-in-time features need history, so a replay runs in two phases:
//  1. warm: every event before the window goes to Kafka (events.login, events.device, events.transaction)
//     and the replay waits until the feature-updater has applied it; nothing is scored. Every warm event
//     precedes every live event, so loading them first cannot leak the future.
//  2. live: the window's events go, in event order, to one worker per account shard. A worker applies a
//     login or device event to the feature store itself (the same idempotent script the feature-updater
//     runs), then publishes it to Kafka for the updater's Postgres sink; it scores a transaction over
//     gRPC, at most Rate per second (the scorer adds it to the store and publishes events.transaction).
//
// Features are per account, so this gives each transaction exactly its account's earlier events and none
// of its later ones. Loading a whole window of later events first does not: device and merchant sightings
// keep only the latest timestamp (ZADD GT), so a later sighting erases an earlier one (docs/DECISIONS.md
// D-025).
package replay

import (
	"bufio"
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/events"
)

// Clock is the time source the pacer uses (injected in tests).
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// RealClock is the wall clock.
var RealClock Clock = realClock{}

// Pacer releases at most rate events per second (no burst). It is used by one goroutine.
type Pacer struct {
	interval time.Duration
	next     time.Time
	clock    Clock
}

// NewPacer returns a pacer; rate <= 0 means unlimited.
func NewPacer(rate float64, clock Clock) *Pacer {
	if clock == nil {
		clock = RealClock
	}
	p := &Pacer{clock: clock}
	if rate > 0 {
		p.interval = time.Duration(float64(time.Second) / rate)
	}
	return p
}

// Wait blocks until the next event may be sent.
func (p *Pacer) Wait(ctx context.Context) error {
	if p.interval == 0 {
		return ctx.Err()
	}
	now := p.clock.Now()
	if p.next.IsZero() || p.next.Before(now) {
		p.next = now
	}
	if d := p.next.Sub(now); d > 0 {
		if err := p.clock.Sleep(ctx, d); err != nil {
			return err
		}
	}
	p.next = p.next.Add(p.interval)
	return nil
}

// Load reads events.jsonl (cmd/gen output), sorted by event_ts.
func Load(path string) ([]events.Event, error) {
	f, err := os.Open(path) // #nosec G304 -- operator-supplied dataset path
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []events.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for line := 1; sc.Scan(); line++ {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e events.Event
		if err := e.UnmarshalJSON(sc.Bytes()); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TSMillis < out[j].TSMillis })
	return out, nil
}

// Split returns the events before the last `window` of the dataset (warm) and inside it (live).
func Split(evs []events.Event, window time.Duration) (warm, live []events.Event) {
	if len(evs) == 0 {
		return nil, nil
	}
	cut := evs[len(evs)-1].TSMillis - window.Milliseconds()
	i := sort.Search(len(evs), func(i int) bool { return evs[i].TSMillis > cut })
	return evs[:i], evs[i:]
}

// Producer is the Kafka producer (kgo.Client).
type Producer interface {
	ProduceSync(ctx context.Context, rs ...*kgo.Record) kgo.ProduceResults
}

// Scorer is the gRPC client (fraudv1.ScorerClient).
type Scorer interface {
	Score(ctx context.Context, in *fraudv1.ScoreRequest, opts ...grpc.CallOption) (*fraudv1.Decision, error)
}

// Config tunes a run.
type Config struct {
	Rate    float64       // transactions scored per second (0 = unlimited)
	Workers int           // concurrent Score calls; accounts are sharded so per-account order holds
	Token   string        // service bearer token
	Timeout time.Duration // per Score call (client side)
	Clock   Clock
	// WaitCaughtUp blocks until the feature-updater has applied everything produced (nil = no wait).
	WaitCaughtUp func(ctx context.Context) error
	// Applier writes live login and device events to the feature store (features.Store). Required when
	// The live window contains them.
	Applier Applier
	Log     *slog.Logger
}

// Applier applies one event to the feature store.
type Applier interface {
	ApplyEvent(ctx context.Context, e *events.Event) error
}

// Stats summarises a run.
type Stats struct {
	Produced    int // events written to Kafka
	Applied     int // live login/device events applied to the feature store
	ApplyErrors int // live login/device events that could not be applied
	Scored      int // successful Score calls
	Errors      int // failed Score calls
	// TransactionIDs lists the transactions scored successfully, so callers can check persistence.
	TransactionIDs []string
	Actions        map[string]int // APPROVE | REVIEW | DECLINE
	Degraded       int
	ByCode         map[string]int // failed calls by gRPC code
	Elapsed        time.Duration  // scoring phase
}

const produceBatch = 1000

func produce(ctx context.Context, p Producer, evs []events.Event, keep func(*events.Event) bool) (int, error) {
	n := 0
	batch := make([]*kgo.Record, 0, produceBatch)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := p.ProduceSync(ctx, batch...).FirstErr(); err != nil {
			return err
		}
		n += len(batch)
		batch = batch[:0]
		return nil
	}
	for i := range evs {
		e := &evs[i]
		if !keep(e) {
			continue
		}
		batch = append(batch, &kgo.Record{Topic: e.TopicFor(), Key: []byte(e.AccountID), Value: e.AppendJSON(nil)})
		if len(batch) == produceBatch {
			if err := flush(); err != nil {
				return n, err
			}
		}
	}
	return n, flush()
}

// Transaction converts a transaction event to the Score request message.
func Transaction(e *events.Event) *fraudv1.Transaction {
	return &fraudv1.Transaction{
		TransactionId: e.EventID, AccountId: e.AccountID, AmountMinor: e.AmountMinor, Currency: e.Currency,
		MerchantId: e.MerchantID, MerchantName: e.MerchantName, Mcc: e.MCC,
		Channel:  fraudv1.Channel(fraudv1.Channel_value["CHANNEL_"+e.Channel]),
		DeviceId: e.DeviceID, IpPrefix: e.IPPrefix, Country: e.Country,
		EventTs: timestamppb.New(time.UnixMilli(e.TSMillis)),
	}
}

// Run executes the warm and live phases.
func Run(ctx context.Context, cfg Config, prod Producer, sc Scorer, warm, live []events.Event) (Stats, error) {
	st := Stats{Actions: map[string]int{}, ByCode: map[string]int{}}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 16
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Second
	}
	if cfg.Applier == nil {
		for i := range live {
			if live[i].Kind != events.KindTransaction {
				return st, fmt.Errorf("live window has %s events but no feature-store Applier is configured", live[i].TopicFor())
			}
		}
	}
	if prod != nil {
		n, err := produce(ctx, prod, warm, func(*events.Event) bool { return true })
		st.Produced += n
		if err != nil {
			return st, fmt.Errorf("warm: %w", err)
		}
		cfg.Log.Info("replay: warm events produced", "count", n)
	}
	if cfg.WaitCaughtUp != nil {
		if err := cfg.WaitCaughtUp(ctx); err != nil {
			return st, fmt.Errorf("wait for feature-updater: %w", err)
		}
	}
	err := runLive(ctx, cfg, prod, sc, live, &st)
	return st, err
}

func shard(account string, n int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(account))
	return int(h.Sum32() % uint32(n)) // #nosec G115 -- n > 0 and small
}

// publisher batches live login/device events to Kafka after the workers applied them.
func publisher(ctx context.Context, prod Producer, in <-chan *kgo.Record, st *Stats, mu *sync.Mutex, log *slog.Logger) {
	batch := make([]*kgo.Record, 0, produceBatch)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		err := prod.ProduceSync(pctx, batch...).FirstErr()
		cancel()
		mu.Lock()
		if err == nil {
			st.Produced += len(batch)
		}
		mu.Unlock()
		if err != nil {
			// The feature store already has these events; only account_events (timelines) misses them.
			log.Warn("replay: publishing live account events failed", "count", len(batch), "err", err)
		}
		batch = batch[:0]
	}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case r, ok := <-in:
			if !ok {
				flush()
				return
			}
			batch = append(batch, r)
			if len(batch) == produceBatch {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

func runLive(ctx context.Context, cfg Config, prod Producer, sc Scorer, live []events.Event, st *Stats) error {
	start := time.Now()
	queues := make([]chan *events.Event, cfg.Workers)
	var mu sync.Mutex
	var wg sync.WaitGroup
	callCtx := ctx
	if cfg.Token != "" {
		callCtx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+cfg.Token)
	}
	var pub chan *kgo.Record
	pubDone := make(chan struct{})
	if prod != nil {
		pub = make(chan *kgo.Record, 4*produceBatch)
		go func() { defer close(pubDone); publisher(ctx, prod, pub, st, &mu, cfg.Log) }()
	} else {
		close(pubDone)
	}
	apply := func(e *events.Event) {
		actx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		err := cfg.Applier.ApplyEvent(actx, e)
		cancel()
		mu.Lock()
		if err != nil {
			st.ApplyErrors++
		} else {
			st.Applied++
		}
		mu.Unlock()
		if err == nil && pub != nil {
			pub <- &kgo.Record{Topic: e.TopicFor(), Key: []byte(e.AccountID), Value: e.AppendJSON(nil)}
		}
	}
	score := func(e *events.Event) {
		cctx, cancel := context.WithTimeout(callCtx, cfg.Timeout)
		d, err := sc.Score(cctx, &fraudv1.ScoreRequest{Transaction: Transaction(e)})
		cancel()
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			st.Errors++
			st.ByCode[status.Code(err).String()]++
			return
		}
		st.Scored++
		st.TransactionIDs = append(st.TransactionIDs, d.TransactionId)
		st.Actions[actionName(d.Action)]++
		if d.Degraded {
			st.Degraded++
		}
	}
	for i := range queues {
		queues[i] = make(chan *events.Event, 64)
		wg.Add(1)
		go func(q <-chan *events.Event) {
			defer wg.Done()
			for e := range q {
				if e.Kind == events.KindTransaction {
					score(e)
				} else {
					apply(e)
				}
			}
		}(queues[i])
	}
	pacer := NewPacer(cfg.Rate, cfg.Clock)
	var runErr error
	for i := range live {
		e := &live[i]
		if e.Kind == events.KindTransaction {
			if err := pacer.Wait(ctx); err != nil {
				runErr = err
				break
			}
		}
		queues[shard(e.AccountID, len(queues))] <- e
	}
	for _, q := range queues {
		close(q)
	}
	wg.Wait()
	if pub != nil {
		close(pub)
	}
	<-pubDone
	st.Elapsed = time.Since(start)
	return runErr
}

func actionName(a fraudv1.Action) string {
	switch a {
	case fraudv1.Action_ACTION_APPROVE:
		return "APPROVE"
	case fraudv1.Action_ACTION_REVIEW:
		return "REVIEW"
	case fraudv1.Action_ACTION_DECLINE:
		return "DECLINE"
	}
	return "UNSPECIFIED"
}
