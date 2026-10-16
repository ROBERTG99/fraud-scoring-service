package api

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
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
type Idempotency interface {
	Get(ctx context.Context, transactionID string) (*fraudv1.Decision, error)
	Put(ctx context.Context, d *fraudv1.Decision) error
}

// DecisionStore persists a decision before the response. Commit returns the stored decision, which is
// an earlier decision when the transaction_id already exists (ON CONFLICT: the first decision wins).
type DecisionStore interface {
	Commit(ctx context.Context, rec store.DecisionRecord) (*fraudv1.Decision, error)
	GetByTransaction(ctx context.Context, transactionID string) (*fraudv1.Decision, error)
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

	if d := s.lookup(ctx, tx.TransactionId); d != nil {
		return d, nil
	}

	asOf := s.d.Now()
	if tx.EventTs != nil {
		asOf = tx.EventTs.AsTime()
	}
	channel := channelNames[tx.Channel]
	ftx := features.Tx{EventID: tx.TransactionId, AccountID: tx.AccountId, TSMillis: asOf.UnixMilli(), AmountMinor: tx.AmountMinor,
		DeviceID: tx.DeviceId, MCC: tx.Mcc, Channel: channel}

	fctx, fcancel := context.WithTimeout(ctx, RedisTimeout)
	vec, _, ferr := s.d.Features.ScoreTx(fctx, ftx)
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
		pred, merr := s.d.Model.Predict(mctx, model.AliasProduction, [][]float32{vec.Float32()})
		mcancel()
		if merr == nil && len(pred.Scores) == 1 {
			score, modelVersion, modelOK = pred.Scores[0], pred.Version, true
		} else {
			telemetry.DegradedTotal.WithLabelValues("model").Inc()
		}
	}

	rs := s.d.Rules.Active()
	env := rules.EnvFromVector(&vec, tx.Mcc, channel, tx.Country, tx.Currency)
	matches := rs.Evaluate(&env)
	res := decide.Combine(rs, decide.Input{Matches: matches, Score: float64(score), ModelOK: modelOK || !featuresOK, FeaturesOK: featuresOK, AmountMinor: tx.AmountMinor})

	featureMap := vec.Map()
	decision := &fraudv1.Decision{
		DecisionId:    newDecisionID(),
		TransactionId: tx.TransactionId,
		Action:        fraudv1.Action(res.Action),
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
			MCC: tx.Mcc, Channel: channel, Country: tx.Country, IPPrefix: tx.IpPrefix, DeviceID: tx.DeviceId, EventTS: asOf.UTC().Format(time.RFC3339Nano)}}
	decision.LatencyMs = float32(time.Since(start).Seconds() * 1000)

	pctx, pcancel := context.WithTimeout(ctx, PostgresTimeout)
	stored, err := s.d.Decisions.Commit(pctx, rec)
	pcancel()
	if errors.Is(err, store.ErrOverloaded) {
		return nil, status.Error(codes.ResourceExhausted, "decision queue full")
	}
	if err != nil {
		// The writer spools to disk when Postgres is down; an error here means even the spool failed.
		s.d.Log.Error("decision not persisted", "transaction_id", tx.TransactionId, "err", err)
		return nil, status.Error(codes.Unavailable, "decision could not be persisted")
	}
	if stored.DecisionId != decision.DecisionId {
		// A concurrent duplicate won the race: return its decision.
		return cloneResponse(stored, false), nil
	}

	ictx, icancel := context.WithTimeout(ctx, RedisTimeout)
	if err := s.d.Idempotency.Put(ictx, decision); err != nil {
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

// lookup returns a stored decision for a replayed transaction_id: Redis first, Postgres when Redis fails.
func (s *ScorerServer) lookup(ctx context.Context, txID string) *fraudv1.Decision {
	ictx, icancel := context.WithTimeout(ctx, RedisTimeout)
	d, err := s.d.Idempotency.Get(ictx, txID)
	icancel()
	if err == nil {
		if d != nil {
			return cloneResponse(d, false)
		}
		return nil
	}
	pctx, pcancel := context.WithTimeout(ctx, PostgresTimeout)
	defer pcancel()
	d, perr := s.d.Decisions.GetByTransaction(pctx, txID)
	if perr != nil && !errors.Is(perr, context.DeadlineExceeded) {
		s.d.Log.Debug("postgres idempotency lookup failed", "err", perr)
	}
	if d != nil {
		return cloneResponse(d, false)
	}
	return nil
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
