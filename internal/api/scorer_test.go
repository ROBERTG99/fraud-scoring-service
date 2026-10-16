package api

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/features"
	"github.com/ROBERTG99/fraud-scoring-service/internal/model"
	"github.com/ROBERTG99/fraud-scoring-service/internal/rules"
	"github.com/ROBERTG99/fraud-scoring-service/internal/store"
)

// ---- in-memory fakes -------------------------------------------------------

type fakeFeatures struct {
	vec   features.Vector
	err   error
	delay time.Duration
	calls int
	mu    sync.Mutex
}

func (f *fakeFeatures) ScoreTx(ctx context.Context, tx features.Tx) (features.Vector, features.Raw, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return features.Vector{}, features.Raw{}, features.ErrUnavailable
		}
	}
	if f.err != nil {
		return features.Vector{}, features.Raw{}, f.err
	}
	v := f.vec
	v[features.IdxAmountMinor] = float64(tx.AmountMinor)
	return v, features.Raw{}, nil
}

func (f *fakeFeatures) Spec() *features.Spec { return features.DefaultSpec() }

type fakeModel struct {
	score   float32
	version string
	err     error
	delay   time.Duration
}

func (m *fakeModel) Predict(ctx context.Context, alias string, rows [][]float32) (model.Prediction, error) {
	if m.delay > 0 {
		select {
		case <-time.After(m.delay):
		case <-ctx.Done():
			return model.Prediction{}, model.ErrUnavailable
		}
	}
	if m.err != nil {
		return model.Prediction{}, m.err
	}
	return model.Prediction{Scores: []float32{m.score}, Version: m.version}, nil
}

type memIdem struct {
	mu   sync.Mutex
	m    map[string]*fraudv1.Decision
	err  error
	gets int
}

func (i *memIdem) Get(_ context.Context, txID string) (*fraudv1.Decision, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.gets++
	if i.err != nil {
		return nil, i.err
	}
	d, ok := i.m[txID]
	if !ok {
		return nil, nil
	}
	return proto.Clone(d).(*fraudv1.Decision), nil
}

func (i *memIdem) Put(_ context.Context, d *fraudv1.Decision) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.err != nil {
		return i.err
	}
	if i.m == nil {
		i.m = map[string]*fraudv1.Decision{}
	}
	if _, ok := i.m[d.TransactionId]; !ok {
		i.m[d.TransactionId] = proto.Clone(d).(*fraudv1.Decision)
	}
	return nil
}

type memDecisions struct {
	mu      sync.Mutex
	byTx    map[string]store.DecisionRecord
	commits int
	err     error
}

func (m *memDecisions) Commit(_ context.Context, rec store.DecisionRecord) (*fraudv1.Decision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.commits++
	if m.err != nil {
		return nil, m.err
	}
	if m.byTx == nil {
		m.byTx = map[string]store.DecisionRecord{}
	}
	if existing, ok := m.byTx[rec.Decision.TransactionId]; ok {
		return existing.Decision, nil // ON CONFLICT: the first decision wins
	}
	m.byTx[rec.Decision.TransactionId] = rec
	return rec.Decision, nil
}

func (m *memDecisions) GetByTransaction(_ context.Context, txID string) (*fraudv1.Decision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.byTx[txID]; ok {
		return r.Decision, nil
	}
	return nil, nil
}

type countingPublisher struct {
	mu  sync.Mutex
	txs []store.DecisionRecord
}

func (p *countingPublisher) PublishTransaction(rec store.DecisionRecord) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.txs = append(p.txs, rec)
}

func testRules(t testing.TB) *rules.Engine {
	t.Helper()
	eng, err := rules.NewEngine("../../rules", nil)
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

type harness struct {
	srv       *ScorerServer
	feats     *fakeFeatures
	model     *fakeModel
	idem      *memIdem
	decisions *memDecisions
	pub       *countingPublisher
}

func newHarness(t testing.TB) *harness {
	h := &harness{
		feats:     &fakeFeatures{vec: features.Vector{0, 1, 3, 4200, 0, 2, 1, 43200, 2, 1, 2, 10, 4.2}},
		model:     &fakeModel{score: 0.62, version: "fraud-gbdt/1"},
		idem:      &memIdem{},
		decisions: &memDecisions{},
		pub:       &countingPublisher{},
	}
	h.srv = NewScorerServer(ScorerDeps{
		Features: h.feats, Model: h.model, Rules: testRules(t), Idempotency: h.idem, Decisions: h.decisions, Publisher: h.pub,
	})
	return h
}

func journeyTx(id string) *fraudv1.Transaction {
	return &fraudv1.Transaction{
		TransactionId: id, AccountId: "acc_00000042", AmountMinor: 94_000, Currency: "USD", MerchantId: "m_00042",
		Mcc: "5732", Channel: fraudv1.Channel_CHANNEL_ECOMMERCE, DeviceId: "ab" + "cdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		IpPrefix: "203.0.113.0/24", Country: "US", EventTs: timestamppb.New(time.Date(2026, 9, 20, 10, 3, 0, 0, time.UTC)),
	}
}

// ---- tests -----------------------------------------------------------------

func TestScoreJourneyDecision(t *testing.T) {
	h := newHarness(t)
	d, err := h.srv.Score(context.Background(), &fraudv1.ScoreRequest{Transaction: journeyTx("0192f000-0000-7000-8000-000000000001"), Explain: true})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != fraudv1.Action_ACTION_REVIEW || d.ModelVersion != "fraud-gbdt/1" || d.RulesVersion != "2026.10.1" || d.Degraded {
		t.Fatalf("decision %+v", d)
	}
	// Spec §3.4 journey: R001 matches and the model scores 0.62.
	want := []string{"NEW_DEVICE_AFTER_CREDENTIAL_CHANGE", "MODEL_SCORE_ELEVATED"}
	if len(d.ReasonCodes) != len(want) {
		t.Fatalf("reasons %v want %v", d.ReasonCodes, want)
	}
	for i := range want {
		if d.ReasonCodes[i] != want[i] {
			t.Fatalf("reasons %v want %v", d.ReasonCodes, want)
		}
	}
	if d.Score < 0.619 || d.Score > 0.621 || d.Features["amount_minor"] != 94_000 || d.DecidedAt == nil || d.DecisionId == "" {
		t.Fatalf("decision fields %+v", d)
	}
	if len(h.pub.txs) != 1 {
		t.Fatalf("published %d transactions", len(h.pub.txs))
	}
}

func TestScoreReplayReturnsSameDecisionID(t *testing.T) {
	h := newHarness(t)
	req := &fraudv1.ScoreRequest{Transaction: journeyTx("0192f000-0000-7000-8000-000000000002")}
	first, err := h.srv.Score(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.srv.Score(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if first.DecisionId != second.DecisionId || first.Action != second.Action {
		t.Fatalf("replay returned a different decision: %s vs %s", first.DecisionId, second.DecisionId)
	}
	if h.feats.calls != 1 || h.decisions.commits != 1 {
		t.Fatalf("replay recomputed: feature calls %d, commits %d", h.feats.calls, h.decisions.commits)
	}
	// Explain on a replay does not change the stored decision.
	if second.Features != nil {
		t.Fatal("features returned without explain")
	}
}

func TestScoreReplayWhenRedisDownUsesPostgres(t *testing.T) {
	h := newHarness(t)
	req := &fraudv1.ScoreRequest{Transaction: journeyTx("0192f000-0000-7000-8000-000000000003")}
	first, err := h.srv.Score(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	h.idem.err = errors.New("redis down")
	h.feats.err = features.ErrUnavailable
	second, err := h.srv.Score(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if first.DecisionId != second.DecisionId {
		t.Fatalf("Postgres idempotency fallback returned a new decision")
	}
}

func TestConcurrentDuplicatesShareOneDecision(t *testing.T) {
	h := newHarness(t)
	req := &fraudv1.ScoreRequest{Transaction: journeyTx("0192f000-0000-7000-8000-000000000004")}
	var wg sync.WaitGroup
	ids := make([]string, 16)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d, err := h.srv.Score(context.Background(), req)
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = d.DecisionId
		}(i)
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("concurrent duplicates got different decision ids: %v", ids)
		}
	}
}

func TestMissingAmountInvalidArgument(t *testing.T) {
	h := newHarness(t)
	tx := journeyTx("0192f000-0000-7000-8000-000000000005")
	tx.AmountMinor = 0
	_, err := h.srv.Score(context.Background(), &fraudv1.ScoreRequest{Transaction: tx})
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
	var fields []string
	for _, d := range st.Details() {
		if br, ok := d.(*errdetails.BadRequest); ok {
			for _, v := range br.FieldViolations {
				fields = append(fields, v.Field)
			}
		}
	}
	if len(fields) != 1 || fields[0] != "transaction.amount_minor" {
		t.Fatalf("field violations %v", fields)
	}
	if h.feats.calls != 0 || h.decisions.commits != 0 {
		t.Fatal("invalid requests must not touch features or storage")
	}
}

func TestValidationRules(t *testing.T) {
	mut := []struct {
		field string
		f     func(*fraudv1.Transaction)
	}{
		{"transaction.transaction_id", func(tx *fraudv1.Transaction) { tx.TransactionId = "not-a-uuid" }},
		{"transaction.account_id", func(tx *fraudv1.Transaction) { tx.AccountId = "alice@example.com" }},
		{"transaction.amount_minor", func(tx *fraudv1.Transaction) { tx.AmountMinor = 10_000_001 }},
		{"transaction.currency", func(tx *fraudv1.Transaction) { tx.Currency = "XAU" }},
		{"transaction.mcc", func(tx *fraudv1.Transaction) { tx.Mcc = "57a2" }},
		{"transaction.channel", func(tx *fraudv1.Transaction) { tx.Channel = fraudv1.Channel_CHANNEL_UNSPECIFIED }},
		{"transaction.device_id", func(tx *fraudv1.Transaction) { tx.DeviceId = "iphone-of-alice" }},
		{"transaction.ip_prefix", func(tx *fraudv1.Transaction) { tx.IpPrefix = "203.0.113.7" }},
		{"transaction.country", func(tx *fraudv1.Transaction) { tx.Country = "usa" }},
		{"transaction.merchant_name", func(tx *fraudv1.Transaction) { tx.MerchantName = string(make([]byte, 121)) }},
	}
	for _, m := range mut {
		tx := journeyTx("0192f000-0000-7000-8000-000000000006")
		m.f(tx)
		v := ValidateTransaction(tx)
		if len(v) == 0 || v[0].Field != m.field {
			t.Errorf("%s: violations %v", m.field, v)
		}
	}
	if v := ValidateTransaction(journeyTx("0192f000-0000-7000-8000-000000000007")); len(v) != 0 {
		t.Fatalf("valid transaction rejected: %v", v)
	}
	if _, err := newHarness(t).srv.Score(context.Background(), &fraudv1.ScoreRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("nil transaction: %v", err)
	}
}
