// Package replay feeds a generated dataset through the running system (spec FR-4, T-31).
//
// Point-in-time features need history, so a replay runs in three phases:
//  1. warm: every event before the window goes to Kafka (events.login, events.device, events.transaction)
//     for the feature-updater; nothing is scored.
//  2. login and device events inside the window go to Kafka too, then the replay waits until the
//     feature-updater has caught up.
//  3. transactions inside the window are scored over gRPC, at most Rate per second, in event order per
//     account (the scorer publishes events.transaction for them).
//
// The feature store reads strictly before each transaction's event_ts, so loading later login and device
// events before scoring never leaks future data into a decision.
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
	Log          *slog.Logger
}

// Stats summarises a run.
type Stats struct {
	Produced int // events written to Kafka
	Scored   int // successful Score calls
	Errors   int // failed Score calls
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

// Run executes the three phases.
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
	if prod != nil {
		n, err := produce(ctx, prod, warm, func(*events.Event) bool { return true })
		st.Produced += n
		if err != nil {
			return st, fmt.Errorf("warm: %w", err)
		}
		n, err = produce(ctx, prod, live, func(e *events.Event) bool { return e.Kind != events.KindTransaction })
		st.Produced += n
		if err != nil {
			return st, fmt.Errorf("live account events: %w", err)
		}
		cfg.Log.Info("replay: events produced", "count", st.Produced)
	}
	if cfg.WaitCaughtUp != nil {
		if err := cfg.WaitCaughtUp(ctx); err != nil {
			return st, fmt.Errorf("wait for feature-updater: %w", err)
		}
	}
	err := score(ctx, cfg, sc, live, &st)
	return st, err
}

func shard(account string, n int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(account))
	return int(h.Sum32() % uint32(n)) // #nosec G115 -- n > 0 and small
}

func score(ctx context.Context, cfg Config, sc Scorer, live []events.Event, st *Stats) error {
	start := time.Now()
	queues := make([]chan *events.Event, cfg.Workers)
	var mu sync.Mutex
	var wg sync.WaitGroup
	callCtx := ctx
	if cfg.Token != "" {
		callCtx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+cfg.Token)
	}
	for i := range queues {
		queues[i] = make(chan *events.Event, 64)
		wg.Add(1)
		go func(q <-chan *events.Event) {
			defer wg.Done()
			for e := range q {
				cctx, cancel := context.WithTimeout(callCtx, cfg.Timeout)
				d, err := sc.Score(cctx, &fraudv1.ScoreRequest{Transaction: Transaction(e)})
				cancel()
				mu.Lock()
				if err != nil {
					st.Errors++
					st.ByCode[status.Code(err).String()]++
				} else {
					st.Scored++
					st.TransactionIDs = append(st.TransactionIDs, d.TransactionId)
					st.Actions[actionName(d.Action)]++
					if d.Degraded {
						st.Degraded++
					}
				}
				mu.Unlock()
			}
		}(queues[i])
	}
	pacer := NewPacer(cfg.Rate, cfg.Clock)
	var runErr error
	for i := range live {
		e := &live[i]
		if e.Kind != events.KindTransaction {
			continue
		}
		if err := pacer.Wait(ctx); err != nil {
			runErr = err
			break
		}
		queues[shard(e.AccountID, len(queues))] <- e
	}
	for _, q := range queues {
		close(q)
	}
	wg.Wait()
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
