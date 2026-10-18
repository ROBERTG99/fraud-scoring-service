package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/decide"
	"github.com/ROBERTG99/fraud-scoring-service/internal/events"
	"github.com/ROBERTG99/fraud-scoring-service/internal/features"
	"github.com/ROBERTG99/fraud-scoring-service/internal/model"
	"github.com/ROBERTG99/fraud-scoring-service/internal/rules"
	"github.com/ROBERTG99/fraud-scoring-service/internal/store"
	"github.com/ROBERTG99/fraud-scoring-service/internal/telemetry"
)

// Timeouts from spec §4.7.
const (
	ScoreTimeout    = 45 * time.Millisecond
	RedisTimeout    = 8 * time.Millisecond
	ModelTimeout    = 20 * time.Millisecond
	PostgresTimeout = 30 * time.Millisecond
)

// FeatureStore reads point-in-time features and adds the transaction atomically.
type FeatureStore interface {
	ScoreTx(ctx context.Context, tx features.Tx) (features.Vector, features.Raw, error)
	Spec() *features.Spec
}

// Idempotency caches decisions by transaction_id (Redis fr:idem:score:{tx_id}). Get returns (nil, nil)
// on a miss.
// Both stores keep the SHA-256 of the request with the decision (RequestHash), so a replay with the same
// transaction_id but different contents is refused instead of answered (audit M6 M3). A nil hash marks an
// entry written before hashes existed; it replays as before.
type Idempotency interface {
	Get(ctx context.Context, transactionID string) (*fraudv1.Decision, []byte, error)
	Put(ctx context.Context, d *fraudv1.Decision, requestHash []byte) error
}

// DecisionStore persists a decision before the response. Commit returns the stored decision, which is
// an earlier decision when the transaction_id already exists (ON CONFLICT: the first decision wins).
type DecisionStore interface {
	Commit(ctx context.Context, rec store.DecisionRecord) (*fraudv1.Decision, error)
	GetByTransaction(ctx context.Context, transactionID string) (*fraudv1.Decision, []byte, error)
}

// Publisher sends events.transaction asynchronously; it must never block.
type Publisher interface {
	PublishTransaction(rec store.DecisionRecord)
}

// ShadowSubmitter scores the shadow model asynchronously; it must never block or fail the request.
type ShadowSubmitter interface {
	Submit(job model.ShadowJob)
}

// ScorerDeps wires the scorer.
type ScorerDeps struct {
	Features    FeatureStore
	Model       model.Client
	Rules       *rules.Engine
	Idempotency Idempotency
	Decisions   DecisionStore
	Publisher   Publisher
	Shadow      ShadowSubmitter
	Live        *LiveStats
	Log         *slog.Logger
	Now         func() time.Time
	MaxEventAge time.Duration // oldest accepted event_ts: 0 → DefaultMaxEventAge, < 0 → no bound (replays)
	Tracer      trace.Tracer  // nil: telemetry.Tracer() (a no-op unless tracing is configured)
}

// ScorerServer implements fraud.v1.Scorer.
type ScorerServer struct {
	fraudv1.UnimplementedScorerServer
	d ScorerDeps
}

// NewScorerServer returns the Score handler.
func NewScorerServer(d ScorerDeps) *ScorerServer {
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Tracer == nil {
		d.Tracer = telemetry.Tracer()
	}
	return &ScorerServer{d: d}
}

var channelNames = map[fraudv1.Channel]string{
	fraudv1.Channel_CHANNEL_CARD_PRESENT: events.ChannelCardPresent,
	fraudv1.Channel_CHANNEL_ECOMMERCE:    events.ChannelEcommerce,
	fraudv1.Channel_CHANNEL_P2P:          events.ChannelP2P,
}

// Score decides one transaction: validate → idempotency → features → model → rules → combine →
// persist → respond; then publish and shadow-score asynchronously.
func (s *ScorerServer) Score(ctx context.Context, req *fraudv1.ScoreRequest) (*fraudv1.Decision, error) {
	start := time.Now()
	if v := ValidateTransaction(req.GetTransaction()); len(v) > 0 {
		return nil, invalidArgument(v)
	}
	ctx, cancel := context.WithTimeout(ctx, ScoreTimeout)
	defer cancel()
	tx := req.GetTransaction()
	ctx, span := s.d.Tracer.Start(ctx, "fraud.score")
	defer span.End()

	reqHash := RequestHash(tx)
	if d, storedHash := s.lookup(ctx, tx.TransactionId); d != nil {
		if err := checkReplay(storedHash, reqHash); err != nil {
			return nil, err
		}
		span.SetAttributes(attribute.Bool("app.idempotent_replay", true))
		decisionAttributes(span, d)
		return d, nil
	}

	asOf := s.d.Now()
	if tx.EventTs != nil {
		ts := tx.EventTs.AsTime()
		if err := s.checkEventTime(ts, asOf); err != nil {
			return nil, err
		}
		asOf = ts
	}
	channel := channelNames[tx.Channel]
	ftx := features.Tx{EventID: tx.TransactionId, AccountID: tx.AccountId, TSMillis: asOf.UnixMilli(), AmountMinor: tx.AmountMinor,
		DeviceID: tx.DeviceId, MCC: tx.Mcc, Channel: channel}

	fctx, fcancel := context.WithTimeout(ctx, RedisTimeout)
	fctx, fspan := s.d.Tracer.Start(fctx, "features.score_tx")
	vec, _, ferr := s.d.Features.ScoreTx(fctx, ftx)
	endSpan(fspan, ferr)
	fcancel()
	featuresOK := ferr == nil
	if !featuresOK {
		vec = features.FallbackVector(s.d.Features.Spec(), ftx)
		telemetry.DegradedTotal.WithLabelValues("features").Inc()
	}

	score := float32(-1)
	modelVersion := "none"
	modelOK := false
	if featuresOK {
		mctx, mcancel := context.WithTimeout(ctx, ModelTimeout)
		mctx, mspan := s.d.Tracer.Start(mctx, "model.predict", modelSpanOpts...)
		pred, merr := s.d.Model.Predict(mctx, model.AliasProduction, [][]float32{vec.Float32()})
		if merr == nil && len(pred.Scores) != 1 {
			merr = errors.New("model returned no score")
		}
		endSpan(mspan, merr)
		mcancel()
		if merr == nil && len(pred.Scores) == 1 {
			score, modelVersion, modelOK = pred.Scores[0], pred.Version, true
		} else {
			telemetry.DegradedTotal.WithLabelValues("model").Inc()
		}
	}

	rs := s.d.Rules.Active()
	_, rspan := s.d.Tracer.Start(ctx, "rules.evaluate")
	env := rules.EnvFromVector(&vec, tx.Mcc, channel, tx.Country, tx.Currency)
	matches := rs.Evaluate(&env)
	res := decide.Combine(rs, decide.Input{Matches: matches, Score: float64(score), ModelOK: modelOK || !featuresOK, FeaturesOK: featuresOK, AmountMinor: tx.AmountMinor})
	if rspan.IsRecording() {
		rspan.SetAttributes(attribute.Int("app.rules.matched", len(matches)))
	}
	rspan.End()

	featureMap := vec.Map()
	decision := &fraudv1.Decision{
		DecisionId:    newDecisionID(),
		TransactionId: tx.TransactionId,
		Action:        protoAction(res.Action),
		Score:         score,
		ReasonCodes:   res.ReasonCodes,
		ModelVersion:  modelVersion,
		RulesVersion:  rs.Version,
		Degraded:      res.Degraded,
		DecidedAt:     timestamppb.New(s.d.Now().UTC()),
	}
	rec := store.DecisionRecord{Decision: decision, AccountID: tx.AccountId, Features: featureMap, AmountMinor: tx.AmountMinor,
		Currency: tx.Currency, Event: transactionEvent(tx, channel, asOf),
		Tx: store.TxAttrs{AmountMinor: tx.AmountMinor, Currency: tx.Currency, MerchantID: defaultMerchant(tx.MerchantId), MerchantName: tx.MerchantName,
			MCC: tx.Mcc, Channel: channel, Country: tx.Country, IPPrefix: tx.IpPrefix, DeviceID: tx.DeviceId, EventTS: asOf.UTC().Format(time.RFC3339Nano)},
		RequestHash: reqHash}
	decision.LatencyMs = float32(time.Since(start).Seconds() * 1000)
	decisionAttributes(span, decision)
	rec.TraceParent = telemetry.Traceparent(ctx) // carried by the outbox to the decisions topic header

	pctx, pcancel := context.WithTimeout(ctx, PostgresTimeout)
	pctx, pspan := s.d.Tracer.Start(pctx, "postgres.commit")
	stored, err := s.d.Decisions.Commit(pctx, rec)
	endSpan(pspan, err)
	pcancel()
	if err != nil {
		span.SetStatus(otelcodes.Error, "decision not persisted")
	}
	if errors.Is(err, store.ErrOverloaded) {
		return nil, status.Error(codes.ResourceExhausted, "decision queue full")
	}
	if err != nil {
		// The writer spools to disk when Postgres is down; an error here means even the spool failed.
		s.d.Log.Error("decision not persisted", "transaction_id", tx.TransactionId, "err", err)
		return nil, status.Error(codes.Unavailable, "decision could not be persisted")
	}
	if stored.DecisionId != decision.DecisionId {
		// The transaction_id was already decided (a concurrent duplicate won the race, or the Redis entry
		// expired): return that decision, unless the request differs from the one it answered.
		gctx, gcancel := context.WithTimeout(ctx, PostgresTimeout)
		_, storedHash, gerr := s.d.Decisions.GetByTransaction(gctx, tx.TransactionId)
		gcancel()
		if gerr != nil {
			// Never return a decision that could not be matched to this request (D-057); the caller retries.
			span.SetStatus(otelcodes.Error, "duplicate not verifiable")
			return nil, status.Error(codes.Unavailable, "decision for this transaction_id could not be verified; retry")
		}
		if err := checkReplay(storedHash, reqHash); err != nil {
			return nil, err
		}
		span.SetAttributes(attribute.Bool("app.idempotent_replay", true))
		decisionAttributes(span, stored) // describe the decision returned, not the discarded one
		return cloneResponse(stored, false), nil
	}

	ictx, icancel := context.WithTimeout(ctx, RedisTimeout)
	if err := s.d.Idempotency.Put(ictx, decision, reqHash); err != nil {
		s.d.Log.Debug("idempotency put failed", "err", err)
	}
	icancel()

	if s.d.Publisher != nil {
		s.d.Publisher.PublishTransaction(rec)
	}
	if s.d.Shadow != nil && featuresOK {
		s.d.Shadow.Submit(model.ShadowJob{DecisionID: decision.DecisionId, Features: vec.Float32(), PrimaryScore: score,
			PrimaryVersion: modelVersion, PrimaryOK: modelOK})
	}

	telemetry.DecisionsTotal.WithLabelValues(store.ActionName(decision.Action)).Inc()
	telemetry.ScoreLatency.Observe(time.Since(start).Seconds())
	// The committed decision is shared with the writer and the idempotency cache: never mutate it.
	out := cloneResponse(decision, false)
	out.LatencyMs = float32(time.Since(start).Seconds() * 1000)
	s.d.Live.Record(out, time.Since(start))
	if req.Explain {
		out.Features = featureMap
	}
	return out, nil
}

// checkEventTime rejects an event_ts outside [now - MaxEventAge, now + MaxEventSkew] (audit M6 H2).
func (s *ScorerServer) checkEventTime(ts, now time.Time) error {
	maxAge := s.d.MaxEventAge
	if maxAge == 0 {
		maxAge = DefaultMaxEventAge
	}
	switch {
	case ts.After(now.Add(MaxEventSkew)):
		return invalidArgument([]*errdetails.BadRequest_FieldViolation{{Field: "transaction.event_ts", Description: "more than 5 minutes in the future"}})
	case maxAge > 0 && ts.Before(now.Add(-maxAge)):
		return invalidArgument([]*errdetails.BadRequest_FieldViolation{{Field: "transaction.event_ts", Description: "older than the accepted window (" + maxAge.String() + ")"}})
	}
	return nil
}

// decisionAttributes records the decision on the Score span (identifiers are synthetic; no account, device
// or merchant attributes).
func decisionAttributes(span trace.Span, d *fraudv1.Decision) {
	if !span.IsRecording() {
		return // no attribute slices built when tracing is off
	}
	span.SetAttributes(
		attribute.String("app.decision.id", d.DecisionId),
		attribute.String("app.transaction_id", d.TransactionId),
		attribute.String("app.decision.action", store.ActionName(d.Action)),
		attribute.Float64("app.decision.score", float64(d.Score)),
		attribute.Bool("app.decision.degraded", d.Degraded),
		attribute.StringSlice("app.decision.reason_codes", d.ReasonCodes),
		attribute.String("app.model_version", d.ModelVersion),
		attribute.String("app.rules_version", d.RulesVersion),
	)
}

// modelSpanOpts is built once: the attribute is the same on every call.
var modelSpanOpts = []trace.SpanStartOption{trace.WithAttributes(attribute.String("app.model_alias", model.AliasProduction))}

func endSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
	}
	span.End()
}

// RequestHash is the SHA-256 of the transaction's deterministic protobuf encoding: identical retries hash
// identically, any changed field changes it.
func RequestHash(tx *fraudv1.Transaction) []byte {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(tx)
	if err != nil {
		return nil
	}
	sum := sha256.Sum256(b)
	return sum[:]
}

// checkReplay refuses a replay whose request differs from the one the stored decision answered. A nil
// stored hash is an entry from before request hashes existed and replays unchecked.
func checkReplay(stored, request []byte) error {
	if stored != nil && !bytes.Equal(stored, request) {
		return status.Error(codes.AlreadyExists, "transaction_id already decided for a different request")
	}
	return nil
}

// lookup returns a stored decision and its request hash: Redis first, Postgres when Redis fails.
func (s *ScorerServer) lookup(ctx context.Context, txID string) (*fraudv1.Decision, []byte) {
	ictx, icancel := context.WithTimeout(ctx, RedisTimeout)
	d, hash, err := s.d.Idempotency.Get(ictx, txID)
	icancel()
	if err == nil {
		if d != nil {
			return cloneResponse(d, false), hash
		}
		return nil, nil
	}
	pctx, pcancel := context.WithTimeout(ctx, PostgresTimeout)
	defer pcancel()
	d, hash, perr := s.d.Decisions.GetByTransaction(pctx, txID)
	if perr != nil && !errors.Is(perr, context.DeadlineExceeded) {
		s.d.Log.Debug("postgres idempotency lookup failed", "err", perr)
	}
	if d != nil {
		return cloneResponse(d, false), hash
	}
	return nil, nil
}

func cloneResponse(d *fraudv1.Decision, withFeatures bool) *fraudv1.Decision {
	out := proto.Clone(d).(*fraudv1.Decision)
	if !withFeatures {
		out.Features = nil
	}
	return out
}

func newDecisionID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}

func transactionEvent(tx *fraudv1.Transaction, channel string, asOf time.Time) *events.Event {
	return &events.Event{Kind: events.KindTransaction, EventID: tx.TransactionId, AccountID: tx.AccountId, TSMillis: asOf.UnixMilli(),
		DeviceID: tx.DeviceId, IPPrefix: tx.IpPrefix, Country: tx.Country, AmountMinor: tx.AmountMinor, Currency: tx.Currency,
		MerchantID: defaultMerchant(tx.MerchantId), MerchantName: tx.MerchantName, MCC: tx.Mcc, Channel: channel}
}

func defaultMerchant(id string) string {
	if strings.TrimSpace(id) == "" {
		return "m_unknown"
	}
	return id
}
