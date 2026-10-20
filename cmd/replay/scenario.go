// The scripted demo scenario (spec §3.4, T-46): `replay -scenario demo -phase <phase>` drives the user
// journeys against a running stack with fresh, run-unique accounts and records one checkpoint per step in a
// JSON state file. scripts/demo.sh runs the phases in order and stops the model server between the
// journey and the degraded phase; tests/e2e/demo.spec.ts then asserts the checkpoints and walks the console.
//
//   - journey: the analyst journey (a USD 940 online purchase from a new device two minutes after an email
//     change is REVIEWed, summarized and queued), a card-testing burst, an invalid event to the DLQ and the
//     shadow comparison.
//   - degraded: with the model server stopped, decisions continue rules-only and the live metrics show it.
//   - recovered: once the model server is back, decisions use the model again.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/events"
	"github.com/ROBERTG99/fraud-scoring-service/internal/features"
	"github.com/ROBERTG99/fraud-scoring-service/internal/replay"
	"github.com/ROBERTG99/fraud-scoring-service/internal/stream"
)

// Phases of the demo scenario, in the order scripts/demo.sh runs them.
const (
	phaseJourney   = "journey"
	phaseDegraded  = "degraded"
	phaseRecovered = "recovered"
)

var demoPhases = []string{phaseJourney, phaseDegraded, phaseRecovered}

// demoCheckpoints lists every checkpoint in order (tests/e2e/demo.spec.ts asserts the same list).
var demoCheckpoints = []string{
	"ato_review", "idempotent_replay", "card_testing_burst", "invalid_event_dlq", "case_persisted",
	"summary_verified", "shadow_compared", "model_unavailable_rules_only", "live_metrics_degraded", "model_recovered",
}

const (
	reasonATO         = "NEW_DEVICE_AFTER_CREDENTIAL_CHANGE"
	reasonCardTesting = "CARD_TESTING_BURST"
	reasonModelDown   = "MODEL_UNAVAILABLE"

	atoAmountMinor     = 94000 // USD 940 (spec §3.4)
	cardTestMinor      = 150   // eight USD 1.50 tests
	cardTestFinalMinor = 200   // then USD 2.00: R003 needs >= 8 in 10 min and <= 300
	smallMinor         = 2500
	degradedReview     = 25000 // above the rules-only REVIEW floor (rules: degraded.model_unavailable)

	atoLatencyTargetMs = 50 // spec §3.4: REVIEW "in < 50 ms"
	scoreBudgetMs      = 45 // Score budget (CLAUDE.md timeouts)
	degradedCalls      = 20
)

// checkpoint is one demo step's outcome.
type checkpoint struct {
	ID     string `json:"id"`
	Phase  string `json:"phase"`
	FR     string `json:"fr"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// demoState is the state file shared by the phases and read by demo.spec.ts.
type demoState struct {
	RunID       string       `json:"run_id"`
	AccountID   string       `json:"account_id"`
	DecisionID  string       `json:"decision_id"`
	CaseID      string       `json:"case_id"`
	Checkpoints []checkpoint `json:"checkpoints"`
}

// loadDemoState starts a fresh run for the journey and continues the journey's run for later phases.
func loadDemoState(path, phase string) (*demoState, error) {
	if phase == phaseJourney {
		return &demoState{}, nil
	}
	b, err := os.ReadFile(path) // #nosec G304 -- operator-supplied state path
	if err != nil {
		return nil, fmt.Errorf("the %s phase needs the journey's state: %w", phase, err)
	}
	var st demoState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &st, nil
}

// merge replaces checkpoints of the same ID and keeps them in demoCheckpoints order.
func (s *demoState) merge(cps []checkpoint) {
	s.Checkpoints = slices.DeleteFunc(s.Checkpoints, func(c checkpoint) bool {
		return slices.ContainsFunc(cps, func(n checkpoint) bool { return n.ID == c.ID })
	})
	s.Checkpoints = append(s.Checkpoints, cps...)
	slices.SortStableFunc(s.Checkpoints, func(a, b checkpoint) int {
		return slices.Index(demoCheckpoints, a.ID) - slices.Index(demoCheckpoints, b.ID)
	})
}

func (s *demoState) allOK() bool {
	if len(s.Checkpoints) != len(demoCheckpoints) {
		return false
	}
	for _, c := range s.Checkpoints {
		if !c.OK {
			return false
		}
	}
	return true
}

func (s *demoState) save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// dlqFinder reports whether a record reached events.dlq.
type dlqFinder interface {
	Find(ctx context.Context, src *kgo.Record) error
}

// demoEnv is what a phase talks to.
type demoEnv struct {
	Scorer       replay.Scorer
	Cases        fraudv1.CaseServiceClient
	Applier      replay.Applier
	Producer     replay.Producer
	DLQ          dlqFinder
	ServiceToken string
	PersonaToken func(ctx context.Context, persona string) (string, error) // dev-issuer JWT
	RunID        string                                                    // lowercase hex, unique per run
	Now          func() time.Time
	Poll         time.Duration // between polls
	Wait         time.Duration // longest wait for an asynchronous result
}

// runDemoPhase runs one phase and returns its checkpoints; it records the demo case in st.
func runDemoPhase(ctx context.Context, env demoEnv, phase string, st *demoState) []checkpoint {
	if st.RunID == "" {
		st.RunID = env.RunID
	}
	d := &demo{env: env, st: st, phase: phase, now: env.Now()}
	switch phase {
	case phaseJourney:
		return d.journey(ctx)
	case phaseDegraded:
		return d.degraded(ctx)
	case phaseRecovered:
		return []checkpoint{d.recovered(ctx)}
	}
	return nil
}

type demo struct {
	env   demoEnv
	st    *demoState
	phase string
	now   time.Time
	seq   int
}

func (d *demo) check(id, fr string, ok bool, format string, args ...any) checkpoint {
	return checkpoint{ID: id, Phase: d.phase, FR: fr, OK: ok, Detail: fmt.Sprintf(format, args...)}
}

// account returns a run-unique account ID for one role of the scenario.
func (d *demo) account(role string) string { return "acc_" + d.env.RunID + role }

// device returns a run-unique device ID shaped like the salted SHA-256 the scorer expects.
func (d *demo) device(role string) string {
	h := sha256.Sum256([]byte("demo:" + d.env.RunID + ":" + d.phase + ":" + role))
	return hex.EncodeToString(h[:])
}

func (d *demo) id() string {
	d.seq++
	return uuid.NewSHA1(uuid.NameSpaceOID, fmt.Appendf(nil, "demo:%s:%s:%d", d.env.RunID, d.phase, d.seq)).String()
}

func (d *demo) tx(account, device string, amount int64, mcc string, at time.Time) *fraudv1.Transaction {
	return &fraudv1.Transaction{TransactionId: d.id(), AccountId: account, AmountMinor: amount, Currency: "USD",
		MerchantId: "m_demo" + mcc, MerchantName: "Demo merchant " + mcc, Mcc: mcc,
		Channel: fraudv1.Channel_CHANNEL_ECOMMERCE, DeviceId: device, IpPrefix: "203.0.113.0/24", Country: "US",
		EventTs: timestamppb.New(at)}
}

func (d *demo) service(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+d.env.ServiceToken)
}

func (d *demo) as(ctx context.Context, persona string) (context.Context, error) {
	tok, err := d.env.PersonaToken(ctx, persona)
	if err != nil {
		return nil, fmt.Errorf("sign in as %s: %w", persona, err)
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok), nil
}

func (d *demo) score(ctx context.Context, tx *fraudv1.Transaction) (*fraudv1.Decision, error) {
	cctx, cancel := context.WithTimeout(d.service(ctx), 2*time.Second)
	defer cancel()
	return d.env.Scorer.Score(cctx, &fraudv1.ScoreRequest{Transaction: tx})
}

// apply writes account events to the feature store, as the feature-updater would, then publishes them so
// the updater's Postgres sink records them for the timeline and the case facts.
func (d *demo) apply(ctx context.Context, evs []events.Event) error {
	recs := make([]*kgo.Record, 0, len(evs))
	for i := range evs {
		actx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := d.env.Applier.ApplyEvent(actx, &evs[i])
		cancel()
		if err != nil {
			return err
		}
		recs = append(recs, &kgo.Record{Topic: evs[i].TopicFor(), Key: []byte(evs[i].AccountID), Value: evs[i].AppendJSON(nil)})
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return d.env.Producer.ProduceSync(pctx, recs...).FirstErr()
}

func (d *demo) sleep(ctx context.Context) error {
	t := time.NewTimer(d.env.Poll)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func describe(dec *fraudv1.Decision) string {
	return fmt.Sprintf("%s in %.1f ms, score %.2f, %s (model %s)", strings.TrimPrefix(dec.Action.String(), "ACTION_"),
		dec.LatencyMs, dec.Score, strings.Join(dec.ReasonCodes, ","), dec.ModelVersion)
}

func (d *demo) journey(ctx context.Context) []checkpoint {
	var out []checkpoint
	ato, atoDec, cp := d.atoReview(ctx)
	out = append(out, cp, d.idempotent(ctx, ato, atoDec), d.cardTesting(ctx), d.invalidEvent(ctx))
	c, cp := d.casePersisted(ctx, atoDec)
	out = append(out, cp, d.summaryVerified(ctx, c, atoDec), d.shadowCompared(ctx))
	return out
}

// takeoverTime places the takeover purchase between 13:00 and 15:00 UTC, where the shipped model scores
// the scripted account in the REVIEW band (docs/DECISIONS.md D-071): now when it is that time, otherwise
// the latest 14:30 UTC, at most 22.5 h back (Score accepts event times from the last 24 h).
func takeoverTime(now time.Time) time.Time {
	now = now.UTC()
	if h := now.Hour(); h == 13 || h == 14 {
		return now
	}
	at := time.Date(now.Year(), now.Month(), now.Day(), 14, 30, 0, 0, time.UTC)
	if at.After(now) {
		at = at.Add(-24 * time.Hour)
	}
	return at
}

// atoReview is steps 1-2 of the analyst journey: an account with a few USD 900 purchases from its home
// device changes its email from a new device, which buys for USD 940 online two minutes later: REVIEW by
// rule R001 within 50 ms.
func (d *demo) atoReview(ctx context.Context) (*fraudv1.Transaction, *fraudv1.Decision, checkpoint) {
	const id, fr = "ato_review", "FR-1, FR-3, FR-7"
	acc, home, attacker := d.account("ato"), d.device("home"), d.device("new")
	d.st.AccountID = acc
	at := takeoverTime(d.now)
	evs := []events.Event{{Kind: events.KindLogin, EventID: d.id(), AccountID: acc, TSMillis: at.Add(-9 * 24 * time.Hour).UnixMilli(),
		DeviceID: home, IPPrefix: "198.51.100.0/24", Country: "US", Success: true, Method: "biometric"}}
	for _, hoursBefore := range []int{20, 14, 8, 3} {
		evs = append(evs, events.Event{Kind: events.KindTransaction, EventID: d.id(), AccountID: acc,
			TSMillis: at.Add(-time.Duration(hoursBefore) * time.Hour).UnixMilli(), DeviceID: home, IPPrefix: "198.51.100.0/24",
			Country: "US", AmountMinor: 90000, Currency: "USD", MerchantID: "m_demo5311", MerchantName: "Demo merchant 5311",
			MCC: "5311", Channel: events.ChannelEcommerce})
	}
	evs = append(evs,
		events.Event{Kind: events.KindLogin, EventID: d.id(), AccountID: acc, TSMillis: at.Add(-3 * time.Minute).UnixMilli(),
			DeviceID: attacker, IPPrefix: "203.0.113.0/24", Country: "US", Success: true, Method: "password"},
		events.Event{Kind: events.KindDevice, EventID: d.id(), AccountID: acc, TSMillis: at.Add(-2 * time.Minute).UnixMilli(),
			DeviceID: attacker, Change: events.ChangeEmailChanged})
	if err := d.apply(ctx, evs); err != nil {
		return nil, nil, d.check(id, fr, false, "account events: %v", err)
	}
	tx := d.tx(acc, attacker, atoAmountMinor, "5732", at)
	dec, err := d.score(ctx, tx)
	if err != nil {
		return tx, nil, d.check(id, fr, false, "Score: %v", err)
	}
	d.st.DecisionID = dec.DecisionId
	ok := dec.Action == fraudv1.Action_ACTION_REVIEW && slices.Contains(dec.ReasonCodes, reasonATO) &&
		!dec.Degraded && dec.LatencyMs < atoLatencyTargetMs
	return tx, dec, d.check(id, fr, ok, "USD 940 online, new device, email changed 2 min earlier: %s (want REVIEW with %s in < %d ms)",
		describe(dec), reasonATO, atoLatencyTargetMs)
}

func (d *demo) idempotent(ctx context.Context, tx *fraudv1.Transaction, first *fraudv1.Decision) checkpoint {
	const id, fr = "idempotent_replay", "FR-1"
	if first == nil {
		return d.check(id, fr, false, "no first decision to replay")
	}
	again, err := d.score(ctx, tx)
	if err != nil {
		return d.check(id, fr, false, "replayed Score: %v", err)
	}
	return d.check(id, fr, again.DecisionId == first.DecisionId, "same transaction_id scored twice: decision %s, then %s",
		first.DecisionId, again.DecisionId)
}

// cardTesting sends eight USD 1.50 transactions in ten minutes, then a USD 2.00 one (rule R003).
func (d *demo) cardTesting(ctx context.Context) checkpoint {
	const id, fr = "card_testing_burst", "FR-7"
	acc, dev := d.account("ct"), d.device("ct")
	var last *fraudv1.Decision
	for i := 8; i >= 0; i-- {
		amount := int64(cardTestMinor)
		if i == 0 {
			amount = cardTestFinalMinor
		}
		dec, err := d.score(ctx, d.tx(acc, dev, amount, "5999", d.now.Add(-time.Duration(i)*time.Minute)))
		if err != nil {
			return d.check(id, fr, false, "Score: %v", err)
		}
		last = dec
	}
	ok := last.Action != fraudv1.Action_ACTION_APPROVE && slices.Contains(last.ReasonCodes, reasonCardTesting)
	return d.check(id, fr, ok, "eight USD 1.50 tests in 10 min, then USD 2.00: %s (want %s)", describe(last), reasonCardTesting)
}

// invalidEvent publishes a login event without its required fields; the feature-updater must dead-letter it.
func (d *demo) invalidEvent(ctx context.Context) checkpoint {
	const id, fr = "invalid_event_dlq", "FR-2"
	rec := &kgo.Record{Topic: stream.TopicLogin, Key: []byte(d.account("ato")),
		Value: fmt.Appendf(nil, `{"schema":%q,"event_id":"demo-invalid-%s","account_id":%q}`, events.SchemaLogin, d.env.RunID, d.account("ato"))}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	res := d.env.Producer.ProduceSync(pctx, rec)
	cancel()
	if err := res.FirstErr(); err != nil {
		return d.check(id, fr, false, "produce: %v", err)
	}
	sent := res[0].Record
	fctx, cancel := context.WithTimeout(ctx, d.env.Wait)
	defer cancel()
	if err := d.env.DLQ.Find(fctx, sent); err != nil {
		return d.check(id, fr, false, "%s partition %d offset %d: %v", sent.Topic, sent.Partition, sent.Offset, err)
	}
	return d.check(id, fr, true, "login event without its required fields (%s partition %d offset %d) is in %s",
		sent.Topic, sent.Partition, sent.Offset, stream.TopicDLQ)
}

// casePersisted finds the REVIEW decision in the analyst's open queue (step 4 starts there).
func (d *demo) casePersisted(ctx context.Context, dec *fraudv1.Decision) (*fraudv1.Case, checkpoint) {
	const id, fr = "case_persisted", "FR-10, FR-13"
	if dec == nil || dec.Action != fraudv1.Action_ACTION_REVIEW {
		return nil, d.check(id, fr, false, "no REVIEW decision to look up")
	}
	actx, err := d.as(ctx, "analyst-1")
	if err != nil {
		return nil, d.check(id, fr, false, "%v", err)
	}
	wctx, cancel := context.WithTimeout(ctx, d.env.Wait)
	defer cancel()
	for {
		c, err := d.findCase(actx, dec.DecisionId)
		if c != nil {
			d.st.CaseID = c.CaseId
			return c, d.check(id, fr, true, "decision %s is open case %s in the review queue", dec.DecisionId, c.CaseId)
		}
		if serr := d.sleep(wctx); serr != nil {
			if err == nil {
				err = errors.New("not in the open queue")
			}
			return nil, d.check(id, fr, false, "decision %s: %v", dec.DecisionId, err)
		}
	}
}

func (d *demo) findCase(ctx context.Context, decisionID string) (*fraudv1.Case, error) {
	token := ""
	for page := 0; page < 50; page++ {
		lctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		resp, err := d.env.Cases.ListCases(lctx, &fraudv1.ListCasesRequest{PageSize: 100, PageToken: token,
			Status: fraudv1.CaseStatus_CASE_STATUS_OPEN, ReasonCode: reasonATO})
		cancel()
		if err != nil {
			return nil, err
		}
		for _, c := range resp.Cases {
			if c.GetDecision().GetDecisionId() == decisionID {
				return c, nil
			}
		}
		if token = resp.NextPageToken; token == "" {
			return nil, nil
		}
	}
	return nil, nil
}

// summaryVerified is step 3: the summarizer's account-takeover summary with three verified evidence items,
// written without touching the decision.
func (d *demo) summaryVerified(ctx context.Context, found *fraudv1.Case, dec *fraudv1.Decision) checkpoint {
	const id, fr = "summary_verified", "FR-11"
	if found == nil {
		return d.check(id, fr, false, "no case to summarize")
	}
	actx, err := d.as(ctx, "analyst-1")
	if err != nil {
		return d.check(id, fr, false, "%v", err)
	}
	start := time.Now()
	wctx, cancel := context.WithTimeout(ctx, d.env.Wait)
	defer cancel()
	var c *fraudv1.Case
	for {
		gctx, gcancel := context.WithTimeout(actx, 2*time.Second)
		c, err = d.env.Cases.GetCase(gctx, &fraudv1.GetCaseRequest{CaseId: found.CaseId})
		gcancel()
		if err == nil && c.SummaryStatus != "" && c.SummaryStatus != "pending" {
			break
		}
		if serr := d.sleep(wctx); serr != nil {
			if err != nil {
				return d.check(id, fr, false, "GetCase: %v", err)
			}
			return d.check(id, fr, false, "summary still %s after %s", c.SummaryStatus, d.env.Wait)
		}
	}
	s := c.GetSummary()
	verified := 0
	for _, e := range s.GetEvidence() {
		if e.Verified {
			verified++
		}
	}
	got := c.GetDecision()
	unchanged := got.GetDecisionId() == dec.DecisionId && got.GetAction() == dec.Action && got.GetScore() == dec.Score &&
		slices.Equal(got.GetReasonCodes(), dec.ReasonCodes) && got.GetModelVersion() == dec.ModelVersion
	ok := c.SummaryStatus == "ok" && s.GetPattern() == "account_takeover" && len(s.GetEvidence()) == 3 && verified == 3 && unchanged
	return d.check(id, fr, ok, "%s after %.1f s: pattern %s, %d of %d evidence items verified, faithfulness %.2f, model %s; decision unchanged: %t",
		c.SummaryStatus, time.Since(start).Seconds(), s.GetPattern(), verified, len(s.GetEvidence()), s.GetFaithfulness(), s.GetModel(), unchanged)
}

// shadowCompared is the risk engineer's view: shadow-model comparisons recorded beside the decisions.
func (d *demo) shadowCompared(ctx context.Context) checkpoint {
	const id, fr = "shadow_compared", "FR-9"
	rctx, err := d.as(ctx, "risk-1")
	if err != nil {
		return d.check(id, fr, false, "%v", err)
	}
	sctx, cancel := context.WithTimeout(rctx, 5*time.Second)
	defer cancel()
	s, err := d.env.Cases.GetShadowSummary(sctx, &fraudv1.GetShadowSummaryRequest{WindowHours: 24})
	if err != nil {
		return d.check(id, fr, false, "GetShadowSummary: %v", err)
	}
	return d.check(id, fr, s.Total > 0, "%d comparisons in 24 h, shadow %s against primary %s, agreement %.1f%%",
		s.Total, s.ShadowVersion, s.PrimaryVersion, 100*s.AgreementRate)
}

// degraded runs while the model server is stopped: every decision still arrives, rules-only, within the
// Score budget, and the live metrics show MODEL_UNAVAILABLE.
func (d *demo) degraded(ctx context.Context) []checkpoint {
	const id, fr = "model_unavailable_rules_only", "FR-8"
	acc, dev := d.account("dg"), d.device("dg")
	rulesOnly, maxMs := 0, 0.0
	var others []string // calls not decided rules-only, for the checkpoint's detail
	var big *fraudv1.Decision
	var failure string
	for i := range degradedCalls {
		amount := int64(smallMinor)
		if i == degradedCalls-1 {
			amount = degradedReview
		}
		dec, err := d.score(ctx, d.tx(acc, dev, amount, "5411", d.now.Add(time.Duration(i-degradedCalls)*time.Second)))
		if err != nil {
			failure = fmt.Sprintf("; call %d failed: %v", i+1, err)
			break
		}
		if dec.Degraded && dec.ModelVersion == "none" && slices.Contains(dec.ReasonCodes, reasonModelDown) {
			rulesOnly++
		} else {
			others = append(others, fmt.Sprintf("call %d: model %s, %s", i+1, dec.ModelVersion, strings.Join(dec.ReasonCodes, ",")))
		}
		maxMs = max(maxMs, float64(dec.LatencyMs))
		big = dec
		// Spread the calls over a few seconds, the way the live metrics would see real traffic.
		if err := d.sleep(ctx); err != nil {
			failure = fmt.Sprintf("; %v", err)
			break
		}
	}
	ok := failure == "" && rulesOnly == degradedCalls && maxMs < scoreBudgetMs && big.Action == fraudv1.Action_ACTION_REVIEW
	bigAction := "none"
	if big != nil {
		bigAction = strings.TrimPrefix(big.Action.String(), "ACTION_")
	}
	out := []checkpoint{d.check(id, fr, ok, "model server stopped: %d of %d decided rules-only (%s, model none), slowest %.1f ms (budget %d), USD 250 → %s%s",
		rulesOnly, degradedCalls, reasonModelDown, maxMs, scoreBudgetMs, bigAction, failure+notRulesOnly(others))}
	return append(out, d.liveMetrics(ctx))
}

func (d *demo) liveMetrics(ctx context.Context) checkpoint {
	const id, fr = "live_metrics_degraded", "FR-8, FR-13"
	actx, err := d.as(ctx, "analyst-1")
	if err != nil {
		return d.check(id, fr, false, "%v", err)
	}
	sctx, cancel := context.WithTimeout(actx, 5*time.Second)
	defer cancel()
	s, err := d.env.Cases.StreamLiveMetrics(sctx, &fraudv1.StreamLiveMetricsRequest{})
	if err != nil {
		return d.check(id, fr, false, "StreamLiveMetrics: %v", err)
	}
	// A snapshot leaves out the still-filling second; the next one (2 s later) has the degraded calls.
	var m *fraudv1.LiveMetrics
	for m == nil || m.ModelUnavailable == 0 {
		next, err := s.Recv()
		if err != nil {
			if m == nil {
				return d.check(id, fr, false, "live metrics: %v", err)
			}
			break
		}
		m = next
	}
	return d.check(id, fr, m.ModelUnavailable > 0, "live metrics (last 10 s): %d %s, degraded rate %.0f%%",
		m.ModelUnavailable, reasonModelDown, 100*m.DegradedRate)
}

// recovered polls until the model answers again after the restart.
func (d *demo) recovered(ctx context.Context) checkpoint {
	const id, fr = "model_recovered", "FR-8"
	acc, dev := d.account("rc"), d.device("rc")
	start := time.Now()
	wctx, cancel := context.WithTimeout(ctx, d.env.Wait)
	defer cancel()
	for calls := 1; ; calls++ {
		dec, err := d.score(ctx, d.tx(acc, dev, smallMinor, "5411", d.now.Add(time.Duration(calls)*time.Millisecond)))
		if err == nil && !dec.Degraded && dec.ModelVersion != "none" {
			return d.check(id, fr, true, "model server back: decision %d used model %s after %.1f s (%s)", calls, dec.ModelVersion,
				time.Since(start).Seconds(), describe(dec))
		}
		if serr := d.sleep(wctx); serr != nil {
			detail := "still degraded"
			if err != nil {
				detail = err.Error()
			}
			return d.check(id, fr, false, "after %d decisions in %s: %s", calls, d.env.Wait, detail)
		}
	}
}

// --- wiring against a running stack

// issuerToken signs in through the dev-issuer (cmd/dev-issuer) as a persona.
func issuerToken(issuer string) func(ctx context.Context, persona string) (string, error) {
	cache := map[string]string{}
	return func(ctx context.Context, persona string) (string, error) {
		if t, ok := cache[persona]; ok {
			return t, nil
		}
		body, _ := json.Marshal(map[string]string{"persona": persona})
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(rctx, http.MethodPost, strings.TrimRight(issuer, "/")+"/token", bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			return "", fmt.Errorf("dev-issuer: %s: %s", resp.Status, b)
		}
		var out struct {
			AccessToken string `json:"access_token"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
			return "", err
		}
		cache[persona] = out.AccessToken
		return out.AccessToken, nil
	}
}

// kafkaDLQ reads events.dlq from the start for the dead letter of one record.
type kafkaDLQ struct{ brokers []string }

func (k kafkaDLQ) Find(ctx context.Context, src *kgo.Record) error {
	cl, err := kgo.NewClient(kgo.SeedBrokers(k.brokers...), kgo.ConsumeTopics(stream.TopicDLQ),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		return err
	}
	defer cl.Close()
	for {
		fs := cl.PollFetches(ctx)
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("not in %s: %w", stream.TopicDLQ, err)
		}
		var found bool
		fs.EachRecord(func(r *kgo.Record) {
			var m stream.DLQMessage
			if json.Unmarshal(r.Value, &m) == nil && m.SourceTopic == src.Topic && m.Partition == src.Partition && m.Offset == src.Offset {
				found = true
			}
		})
		if found {
			return nil
		}
	}
}

func newRunID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// runDemo runs one phase against the stack, prints its checkpoints and saves the state file.
func runDemo(ctx context.Context, o options, log *slog.Logger) (bool, error) {
	st, err := loadDemoState(o.state, o.phase)
	if err != nil {
		return false, err
	}
	brokers := stream.Brokers(o.brokers)
	kcl, err := kgo.NewClient(kgo.SeedBrokers(brokers...), kgo.RecordPartitioner(kgo.StickyKeyPartitioner(nil)))
	if err != nil {
		return false, err
	}
	defer kcl.Close()
	conn, err := grpc.NewClient(o.scorer, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close() }()
	spec, err := features.LoadSpec(envOr("FRAUD_FEATURE_SPEC", "features/spec.yaml"))
	if err != nil {
		return false, err
	}
	rdb := features.NewUpdaterRedisClient(envOr("FRAUD_REDIS_ADDR", "127.0.0.1:8530"))
	defer func() { _ = rdb.Close() }()
	env := demoEnv{Scorer: fraudv1.NewScorerClient(conn), Cases: fraudv1.NewCaseServiceClient(conn),
		Applier: features.NewStore(rdb, os.Getenv("FRAUD_REDIS_PREFIX"), spec), Producer: kcl, DLQ: kafkaDLQ{brokers: brokers},
		ServiceToken: o.token, PersonaToken: issuerToken(o.issuer), RunID: newRunID(), Now: time.Now,
		Poll: 250 * time.Millisecond, Wait: 60 * time.Second}
	log.Info("demo: phase starting", "phase", o.phase, "run", env.RunID)
	cps := runDemoPhase(ctx, env, o.phase, st)
	st.merge(cps)
	if err := st.save(o.state); err != nil {
		return false, err
	}
	ok := true
	for _, c := range cps {
		mark := "OK  "
		if !c.OK {
			mark, ok = "FAIL", false
		}
		fmt.Printf("demo: %s %d/%d %s (%s): %s\n", mark, slices.Index(demoCheckpoints, c.ID)+1, len(demoCheckpoints), c.ID, c.FR, c.Detail)
	}
	return ok, nil
}

// notRulesOnly lists the calls of the degraded phase that were not decided rules-only (at most three).
func notRulesOnly(others []string) string {
	if len(others) == 0 {
		return ""
	}
	return "; not rules-only: " + strings.Join(others[:min(len(others), 3)], "; ")
}
