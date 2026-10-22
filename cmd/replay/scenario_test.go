package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/api"
	"github.com/ROBERTG99/fraud-scoring-service/internal/events"
	"github.com/ROBERTG99/fraud-scoring-service/internal/stream"
)

var demoNow = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

// fakeScorer decides by amount, the way the rules would for the scripted transactions, and returns the
// stored decision for a repeated transaction ID. degraded makes every call MODEL_UNAVAILABLE.
type fakeScorer struct {
	mu        sync.Mutex
	byTx      map[string]*fraudv1.Decision
	degraded  func(call int) bool
	decide    func(tx *fraudv1.Transaction) (fraudv1.Action, []string)
	calls     int
	tokens    []string
	requests  []*fraudv1.Transaction
	decisions []*fraudv1.Decision
}

func newFakeScorer() *fakeScorer {
	return &fakeScorer{byTx: map[string]*fraudv1.Decision{}, degraded: func(int) bool { return false },
		decide: func(tx *fraudv1.Transaction) (fraudv1.Action, []string) {
			switch tx.AmountMinor {
			case atoAmountMinor:
				return fraudv1.Action_ACTION_REVIEW, []string{"NEW_DEVICE_AFTER_CREDENTIAL_CHANGE", "MODEL_SCORE_ELEVATED"}
			case cardTestFinalMinor:
				return fraudv1.Action_ACTION_REVIEW, []string{"CARD_TESTING_BURST"}
			}
			return fraudv1.Action_ACTION_APPROVE, nil
		}}
}

func (f *fakeScorer) Score(ctx context.Context, in *fraudv1.ScoreRequest, _ ...grpc.CallOption) (*fraudv1.Decision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	md, _ := metadata.FromOutgoingContext(ctx)
	f.tokens = append(f.tokens, strings.Join(md.Get("authorization"), ","))
	f.requests = append(f.requests, in.Transaction)
	if d, ok := f.byTx[in.Transaction.TransactionId]; ok {
		return d, nil
	}
	f.calls++
	action, codes := f.decide(in.Transaction)
	d := &fraudv1.Decision{DecisionId: fmt.Sprintf("0192f0a0-0000-7000-8000-%012d", f.calls),
		TransactionId: in.Transaction.TransactionId, Action: action, Score: 0.62, ReasonCodes: codes,
		ModelVersion: "fraud-gbdt/2", RulesVersion: "2026.10.2", LatencyMs: 7.5}
	if f.degraded(f.calls) {
		d.Degraded, d.ModelVersion, d.Score, d.LatencyMs = true, "none", -1, 21
		d.ReasonCodes = append([]string{"MODEL_UNAVAILABLE"}, d.ReasonCodes...)
		if in.Transaction.AmountMinor >= 20000 {
			d.Action = fraudv1.Action_ACTION_REVIEW
		}
	}
	f.byTx[in.Transaction.TransactionId] = d
	f.decisions = append(f.decisions, d)
	return d, nil
}

// fakeCases serves the case for the scripted REVIEW decision once the scorer made it; the summary is
// pending on the first GetCase and ready afterwards.
type fakeCases struct {
	fraudv1.CaseServiceClient
	scorer   *fakeScorer
	summary  *fraudv1.CaseSummary
	gets     int
	shadow   int64
	live     int64
	personas []string
}

func (f *fakeCases) persona(ctx context.Context) {
	md, _ := metadata.FromOutgoingContext(ctx)
	f.personas = append(f.personas, strings.Join(md.Get("authorization"), ","))
}

func (f *fakeCases) reviewCase() *fraudv1.Case {
	f.scorer.mu.Lock()
	defer f.scorer.mu.Unlock()
	for _, d := range f.scorer.decisions {
		if slices.Contains(d.ReasonCodes, "NEW_DEVICE_AFTER_CREDENTIAL_CHANGE") && d.Action == fraudv1.Action_ACTION_REVIEW {
			return &fraudv1.Case{CaseId: "5b0e8f6c-1d2e-4f3a-9b8c-7d6e5f4a3b2c", Decision: d, Status: fraudv1.CaseStatus_CASE_STATUS_OPEN}
		}
	}
	return nil
}

func (f *fakeCases) ListCases(ctx context.Context, in *fraudv1.ListCasesRequest, _ ...grpc.CallOption) (*fraudv1.ListCasesResponse, error) {
	f.persona(ctx)
	out := &fraudv1.ListCasesResponse{Cases: []*fraudv1.Case{{CaseId: "00000000-0000-4000-8000-000000000001",
		Decision: &fraudv1.Decision{DecisionId: "other"}}}}
	if c := f.reviewCase(); c != nil && in.ReasonCode == "NEW_DEVICE_AFTER_CREDENTIAL_CHANGE" {
		if in.PageToken == "" {
			out.NextPageToken = "p2"
			return out, nil
		}
		out.Cases = append(out.Cases, c)
	}
	return out, nil
}

func (f *fakeCases) GetCase(ctx context.Context, in *fraudv1.GetCaseRequest, _ ...grpc.CallOption) (*fraudv1.Case, error) {
	f.persona(ctx)
	c := f.reviewCase()
	if c == nil || c.CaseId != in.CaseId {
		return nil, errors.New("not found")
	}
	f.gets++
	c.SummaryStatus = "pending"
	if f.gets > 1 {
		c.SummaryStatus, c.Summary = f.summary.Status, f.summary
	}
	return c, nil
}

func (f *fakeCases) GetShadowSummary(ctx context.Context, _ *fraudv1.GetShadowSummaryRequest, _ ...grpc.CallOption) (*fraudv1.ShadowSummary, error) {
	f.persona(ctx)
	return &fraudv1.ShadowSummary{PrimaryVersion: "fraud-gbdt/2", ShadowVersion: "fraud-gbdt/2", Total: f.shadow, AgreementRate: 1}, nil
}

type liveStream struct {
	grpc.ClientStream
	msgs []*fraudv1.LiveMetrics
}

func (s *liveStream) Recv() (*fraudv1.LiveMetrics, error) {
	if len(s.msgs) == 0 {
		return nil, io.EOF
	}
	m := s.msgs[0]
	s.msgs = s.msgs[1:]
	return m, nil
}

func (f *fakeCases) StreamLiveMetrics(ctx context.Context, _ *fraudv1.StreamLiveMetricsRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[fraudv1.LiveMetrics], error) {
	f.persona(ctx)
	// The first snapshot leaves out the still-filling second, so the degraded calls show from the next one.
	return &liveStream{msgs: []*fraudv1.LiveMetrics{{}, {ModelUnavailable: f.live, DegradedRate: 1}}}, nil
}

func verifiedSummary() *fraudv1.CaseSummary {
	return &fraudv1.CaseSummary{Status: "ok", Pattern: "account_takeover", Model: "fake", Faithfulness: 1,
		Evidence: []*fraudv1.Evidence{
			{FactKey: "feature.new_device", Statement: "The device is new.", Verified: true},
			{FactKey: "feature.minutes_since_email_change", Statement: "Email changed 2 minutes earlier.", Verified: true},
			{FactKey: "feature.amount_minor", Statement: "The amount is 94000.", Verified: true},
		}}
}

type fakeApplier struct {
	mu  sync.Mutex
	evs []events.Event
}

func (a *fakeApplier) ApplyEvent(_ context.Context, e *events.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.evs = append(a.evs, *e)
	return nil
}

type fakeProducer struct {
	mu      sync.Mutex
	records []*kgo.Record
}

func (p *fakeProducer) ProduceSync(_ context.Context, rs ...*kgo.Record) kgo.ProduceResults {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(kgo.ProduceResults, 0, len(rs))
	for _, r := range rs {
		r.Partition, r.Offset = 3, int64(len(p.records))
		p.records = append(p.records, r)
		out = append(out, kgo.ProduceResult{Record: r})
	}
	return out
}

type fakeDLQ struct {
	found bool
	asked []*kgo.Record
}

func (d *fakeDLQ) Find(_ context.Context, src *kgo.Record) error {
	d.asked = append(d.asked, src)
	if !d.found {
		return errors.New("not in events.dlq")
	}
	return nil
}

type demoFixture struct {
	env     demoEnv
	scorer  *fakeScorer
	cases   *fakeCases
	applier *fakeApplier
	prod    *fakeProducer
	dlq     *fakeDLQ
}

func newDemoFixture() *demoFixture {
	sc := newFakeScorer()
	f := &demoFixture{scorer: sc, cases: &fakeCases{scorer: sc, summary: verifiedSummary(), shadow: 120, live: 20},
		applier: &fakeApplier{}, prod: &fakeProducer{}, dlq: &fakeDLQ{found: true}}
	f.env = demoEnv{Scorer: sc, Cases: f.cases, Applier: f.applier, Producer: f.prod, DLQ: f.dlq,
		ServiceToken: "svc-token", RunID: "a1b2c3d4e5f6",
		PersonaToken: func(_ context.Context, persona string) (string, error) { return "jwt-" + persona, nil },
		Now:          func() time.Time { return demoNow }, Poll: time.Millisecond, Wait: time.Second}
	return f
}

func byID(cps []checkpoint) map[string]checkpoint {
	m := map[string]checkpoint{}
	for _, c := range cps {
		m[c.ID] = c
	}
	return m
}

func TestDemoJourneyPassesEveryCheckpointOnAHealthyStack(t *testing.T) {
	f := newDemoFixture()
	st := &demoState{}
	cps := runDemoPhase(context.Background(), f.env, phaseJourney, st)
	var ids []string
	for _, c := range cps {
		ids = append(ids, c.ID)
		if !c.OK {
			t.Errorf("%s failed: %s", c.ID, c.Detail)
		}
	}
	want := []string{"ato_review", "idempotent_replay", "card_testing_burst", "invalid_event_dlq", "case_persisted",
		"summary_verified", "shadow_compared"}
	if !slices.Equal(ids, want) {
		t.Fatalf("journey checkpoints = %v, want %v", ids, want)
	}
	if st.CaseID != "5b0e8f6c-1d2e-4f3a-9b8c-7d6e5f4a3b2c" || st.AccountID == "" || st.DecisionID == "" {
		t.Fatalf("state = %+v, want the demo case, account and decision", st)
	}
	for _, tok := range f.scorer.tokens {
		if tok != "Bearer svc-token" {
			t.Fatalf("Score sent authorization %q, want the service token", tok)
		}
	}
	if !slices.Contains(f.cases.personas, "Bearer jwt-analyst-1") || !slices.Contains(f.cases.personas, "Bearer jwt-risk-1") {
		t.Fatalf("console calls used %v, want the analyst and risk-engineer personas", f.cases.personas)
	}
}

func TestDemoATOStepsFollowTheUserJourney(t *testing.T) {
	f := newDemoFixture()
	runDemoPhase(context.Background(), f.env, phaseJourney, &demoState{})
	// The account's login from a new device and its email change come before the USD 940 purchase.
	var ato *fraudv1.Transaction
	for _, tx := range f.scorer.requests {
		if tx.AmountMinor == atoAmountMinor {
			ato = tx
			break
		}
	}
	if ato == nil || ato.Currency != "USD" || ato.Channel != fraudv1.Channel_CHANNEL_ECOMMERCE {
		t.Fatalf("ATO transaction = %+v, want USD 940 online", ato)
	}
	var change *events.Event
	for i := range f.applier.evs {
		e := &f.applier.evs[i]
		if e.AccountID == ato.AccountId && e.Change == events.ChangeEmailChanged {
			change = e
		}
		if e.AccountID == ato.AccountId && e.Change == events.ChangeDeviceBound {
			t.Fatalf("the takeover account binds a device (%+v); the summary would read it as the legitimate twin", e)
		}
	}
	if change == nil {
		t.Fatal("no email change applied for the takeover account")
	}
	if got := ato.EventTs.AsTime().Sub(time.UnixMilli(change.TSMillis)); got != 2*time.Minute {
		t.Fatalf("purchase comes %s after the email change, want 2m", got)
	}
	if change.DeviceID != ato.DeviceId {
		t.Fatal("the email change and the purchase should come from the same new device")
	}
}

func TestDemoTakeoverHappensInTheAfternoonWithinScoresWindow(t *testing.T) {
	// The model's score depends on the hour; between 13:00 and 15:00 UTC the scripted takeover lands in the
	// REVIEW band (0.30-0.85), so the purchase is placed there, never more than 23 h back (Score's bound is 24 h).
	for h := 0; h < 24; h++ {
		for _, m := range []int{0, 29, 31, 59} {
			now := time.Date(2026, 10, 19, h, m, 0, 0, time.UTC)
			at := takeoverTime(now)
			if at.After(now) || now.Sub(at) > 23*time.Hour || at.Hour() < 13 || at.Hour() > 14 {
				t.Fatalf("takeoverTime(%s) = %s, want 13:00-14:59 UTC within the last 23 h", now.Format("15:04"), at)
			}
		}
	}
}

func TestDemoTakeoverAccountHasHistory(t *testing.T) {
	f := newDemoFixture()
	runDemoPhase(context.Background(), f.env, phaseJourney, &demoState{})
	at := takeoverTime(demoNow)
	var history int
	for _, e := range f.applier.evs {
		if e.Kind == events.KindTransaction && strings.HasSuffix(e.AccountID, "ato") {
			history++
			if age := at.Sub(time.UnixMilli(e.TSMillis)); age <= 0 || age >= 24*time.Hour || e.MCC == "5732" {
				t.Fatalf("history purchase %+v: want the 24 h before the takeover, at another merchant category", e)
			}
		}
	}
	if history != 4 {
		t.Fatalf("%d history purchases applied, want 4", history)
	}
}

func TestDemoScriptedEventsAndTransactionsAreValid(t *testing.T) {
	f := newDemoFixture()
	runDemoPhase(context.Background(), f.env, phaseJourney, &demoState{})
	for _, tx := range f.scorer.requests {
		if v := api.ValidateTransaction(tx); len(v) > 0 {
			t.Fatalf("transaction %s is invalid: %v", tx.TransactionId, v)
		}
		if d := demoNow.Sub(tx.EventTs.AsTime()); d < 0 || d > 23*time.Hour {
			t.Fatalf("transaction %s is %s old; Score accepts the last 24 h only", tx.TransactionId, d)
		}
	}
	v := stream.MustValidator()
	invalid := 0
	for _, r := range f.prod.records {
		if err := v.ValidateTopic(r.Topic, r.Value); err != nil {
			invalid++
			if r.Topic != stream.TopicLogin {
				t.Fatalf("record on %s is invalid: %v", r.Topic, err)
			}
		}
	}
	if invalid != 1 {
		t.Fatalf("%d invalid records produced, want exactly the one DLQ probe", invalid)
	}
}

func TestDemoATOReviewFailsWhenTheDecisionIsNotReview(t *testing.T) {
	f := newDemoFixture()
	f.scorer.decide = func(*fraudv1.Transaction) (fraudv1.Action, []string) { return fraudv1.Action_ACTION_APPROVE, nil }
	cps := byID(runDemoPhase(context.Background(), f.env, phaseJourney, &demoState{}))
	for _, id := range []string{"ato_review", "card_testing_burst", "case_persisted", "summary_verified"} {
		if cps[id].OK {
			t.Errorf("%s passed with every transaction approved", id)
		}
	}
	if !strings.Contains(cps["ato_review"].Detail, "APPROVE") {
		t.Errorf("ato_review detail %q does not say what came back", cps["ato_review"].Detail)
	}
}

func TestDemoATOReviewFailsOverTheLatencyTarget(t *testing.T) {
	f := newDemoFixture()
	f.env.Scorer = scorerFunc(func(ctx context.Context, in *fraudv1.ScoreRequest) (*fraudv1.Decision, error) {
		d, err := f.scorer.Score(ctx, in)
		if err == nil && in.Transaction.AmountMinor == atoAmountMinor {
			d.LatencyMs = 51
		}
		return d, err
	})
	if cps := byID(runDemoPhase(context.Background(), f.env, phaseJourney, &demoState{})); cps["ato_review"].OK {
		t.Fatal("ato_review passed at 51 ms; the journey promises a decision in under 50 ms")
	}
}

type scorerFunc func(ctx context.Context, in *fraudv1.ScoreRequest) (*fraudv1.Decision, error)

func (f scorerFunc) Score(ctx context.Context, in *fraudv1.ScoreRequest, _ ...grpc.CallOption) (*fraudv1.Decision, error) {
	return f(ctx, in)
}

func TestDemoSummaryMustBeVerifiedTakeover(t *testing.T) {
	for name, mutate := range map[string]func(s *fraudv1.CaseSummary){
		"unverified evidence": func(s *fraudv1.CaseSummary) { s.Evidence[1].Verified = false },
		"two evidence items":  func(s *fraudv1.CaseSummary) { s.Evidence = s.Evidence[:2] },
		"other pattern":       func(s *fraudv1.CaseSummary) { s.Pattern = "unclear" },
		"fallback":            func(s *fraudv1.CaseSummary) { s.Status = "fallback" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newDemoFixture()
			mutate(f.cases.summary)
			if cps := byID(runDemoPhase(context.Background(), f.env, phaseJourney, &demoState{})); cps["summary_verified"].OK {
				t.Fatalf("summary_verified passed with %s", name)
			}
		})
	}
}

func TestDemoSummaryTimesOutWhilePending(t *testing.T) {
	f := newDemoFixture()
	f.cases.summary = &fraudv1.CaseSummary{Status: "pending"}
	f.env.Wait = 20 * time.Millisecond
	cps := byID(runDemoPhase(context.Background(), f.env, phaseJourney, &demoState{}))
	if cps["summary_verified"].OK || !strings.Contains(cps["summary_verified"].Detail, "pending") {
		t.Fatalf("summary_verified = %+v, want a failure that says the summary stayed pending", cps["summary_verified"])
	}
}

func TestDemoInvalidEventMustReachTheDLQ(t *testing.T) {
	f := newDemoFixture()
	f.dlq.found = false
	cps := byID(runDemoPhase(context.Background(), f.env, phaseJourney, &demoState{}))
	if cps["invalid_event_dlq"].OK {
		t.Fatal("invalid_event_dlq passed though the record never reached events.dlq")
	}
	if len(f.dlq.asked) != 1 || f.dlq.asked[0].Topic != stream.TopicLogin {
		t.Fatalf("DLQ lookups = %v, want one for the events.login probe", f.dlq.asked)
	}
}

func TestDemoShadowNeedsComparisons(t *testing.T) {
	f := newDemoFixture()
	f.cases.shadow = 0
	if cps := byID(runDemoPhase(context.Background(), f.env, phaseJourney, &demoState{})); cps["shadow_compared"].OK {
		t.Fatal("shadow_compared passed with no comparisons recorded")
	}
}

func TestDemoDegradedPhaseShowsRulesOnlyDecisions(t *testing.T) {
	f := newDemoFixture()
	f.scorer.degraded = func(int) bool { return true }
	cps := runDemoPhase(context.Background(), f.env, phaseDegraded, &demoState{})
	if len(cps) != 2 || cps[0].ID != "model_unavailable_rules_only" || cps[1].ID != "live_metrics_degraded" {
		t.Fatalf("degraded checkpoints = %+v", cps)
	}
	for _, c := range cps {
		if !c.OK {
			t.Errorf("%s failed: %s", c.ID, c.Detail)
		}
	}
	if len(f.scorer.requests) < 10 {
		t.Fatalf("%d transactions scored while the model was down, want a run the live metrics can see", len(f.scorer.requests))
	}
}

func TestDemoDegradedPhaseFailsWhenTheModelStillAnswers(t *testing.T) {
	f := newDemoFixture()
	f.cases.live = 0
	cps := byID(runDemoPhase(context.Background(), f.env, phaseDegraded, &demoState{}))
	if cps["model_unavailable_rules_only"].OK || cps["live_metrics_degraded"].OK {
		t.Fatalf("degraded phase passed with a healthy model: %+v", cps)
	}
}

func TestDemoRecoveredPhaseWaitsForTheModel(t *testing.T) {
	f := newDemoFixture()
	f.scorer.degraded = func(call int) bool { return call <= 2 }
	cps := runDemoPhase(context.Background(), f.env, phaseRecovered, &demoState{})
	if len(cps) != 1 || cps[0].ID != "model_recovered" || !cps[0].OK {
		t.Fatalf("recovered checkpoints = %+v, want model_recovered OK after two degraded calls", cps)
	}
	f = newDemoFixture()
	f.scorer.degraded = func(int) bool { return true }
	f.env.Wait = 20 * time.Millisecond
	if cps := runDemoPhase(context.Background(), f.env, phaseRecovered, &demoState{}); cps[0].OK {
		t.Fatal("model_recovered passed while every call stayed degraded")
	}
}

func TestDemoStateKeepsCheckpointOrderAcrossPhases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "demo", "checkpoints.json")
	f := newDemoFixture()
	for _, phase := range []string{phaseJourney, phaseDegraded, phaseRecovered} {
		f.scorer.degraded = func(int) bool { return phase == phaseDegraded }
		st, err := loadDemoState(path, phase)
		if err != nil {
			t.Fatal(err)
		}
		st.merge(runDemoPhase(context.Background(), f.env, phase, st))
		if err := st.save(path); err != nil {
			t.Fatal(err)
		}
	}
	st, err := loadDemoState(path, phaseRecovered)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range st.Checkpoints {
		ids = append(ids, c.ID)
	}
	if !slices.Equal(ids, demoCheckpoints) {
		t.Fatalf("checkpoints = %v, want %v", ids, demoCheckpoints)
	}
	if !st.allOK() || st.CaseID == "" {
		t.Fatalf("state = %+v, want every checkpoint OK and the case kept", st)
	}
	// A new journey starts a new run: the old checkpoints and case are dropped.
	st, err = loadDemoState(path, phaseJourney)
	if err != nil || len(st.Checkpoints) != 0 || st.CaseID != "" {
		t.Fatalf("journey state = %+v, %v; want a fresh run", st, err)
	}
	if _, err := loadDemoState(filepath.Join(t.TempDir(), "missing.json"), phaseDegraded); err == nil {
		t.Fatal("the degraded phase ran without a journey's state")
	}
}

func TestDemoFlagsSelectTheScenario(t *testing.T) {
	o, err := parse([]string{"-token", "t", "-scenario", "demo", "-phase", "degraded", "-state", "x.json", "-issuer", "http://i"})
	if err != nil {
		t.Fatal(err)
	}
	if o.scenario != "demo" || o.phase != phaseDegraded || o.state != "x.json" || o.issuer != "http://i" {
		t.Fatalf("options = %+v", o)
	}
	for _, bad := range [][]string{
		{"-token", "t", "-scenario", "other"},
		{"-token", "t", "-scenario", "demo", "-phase", "later"},
	} {
		if _, err := parse(bad); err == nil {
			t.Errorf("parse(%v) accepted an unknown scenario or phase", bad)
		}
	}
}

// When a call is not decided rules-only, the checkpoint says which call and what came back, so a CI failure
// can be told apart from a demo bug (CI once saw 19 of 20 with no clue which one).
func TestDemoDegradedPhaseNamesTheCallThatWasNotRulesOnly(t *testing.T) {
	f := newDemoFixture()
	f.scorer.degraded = func(call int) bool { return call != 7 }
	cps := byID(runDemoPhase(context.Background(), f.env, phaseDegraded, &demoState{}))
	c := cps["model_unavailable_rules_only"]
	if c.OK || !strings.Contains(c.Detail, "call 7: ") || !strings.Contains(c.Detail, "model fraud-gbdt") {
		t.Fatalf("checkpoint = %+v, want a failure naming call 7 and its model", c)
	}
}
