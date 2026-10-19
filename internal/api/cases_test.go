package api

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/auth"
	"github.com/ROBERTG99/fraud-scoring-service/internal/store"
)

const testCaseID = "0192f000-0000-7000-8000-0000000000aa"

type fakeBackend struct {
	mu      sync.Mutex
	labels  []store.LabelInput
	samples []store.FeatureSample
	lastQ   store.ListQuery
}

func (f *fakeBackend) ListCases(_ context.Context, q store.ListQuery) ([]*fraudv1.Case, string, int32, error) {
	f.mu.Lock()
	f.lastQ = q
	f.mu.Unlock()
	if q.PageToken == "bad" {
		return nil, "", 0, store.ErrBadPageToken
	}
	return []*fraudv1.Case{{CaseId: testCaseID}}, "next", 1, nil
}

func (f *fakeBackend) GetCase(_ context.Context, id string) (*fraudv1.Case, error) {
	if id != testCaseID {
		return nil, store.ErrNotFound
	}
	return &fraudv1.Case{CaseId: id}, nil
}

func (f *fakeBackend) LabelCase(_ context.Context, in store.LabelInput) (*fraudv1.Case, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range f.labels {
		if l.CaseID == in.CaseID && l.Label != in.Label {
			return nil, store.ErrConflict
		}
	}
	f.labels = append(f.labels, in)
	return &fraudv1.Case{CaseId: in.CaseID, Status: fraudv1.CaseStatus_CASE_STATUS_LABELLED}, nil
}

func (f *fakeBackend) Timeline(_ context.Context, acc string, _ int) ([]*fraudv1.TimelineItem, error) {
	return []*fraudv1.TimelineItem{{Kind: "decision"}}, nil
}

func (f *fakeBackend) ShadowSummary(_ context.Context, _, _ int) (*fraudv1.ShadowSummary, error) {
	return &fraudv1.ShadowSummary{Total: 3}, nil
}

func (f *fakeBackend) RecentSamples(_ context.Context, n int) ([]store.FeatureSample, error) {
	return f.samples[:min(n, len(f.samples))], nil
}

func newCaseServer(t testing.TB) (*CaseServer, *fakeBackend) {
	b := &fakeBackend{samples: []store.FeatureSample{
		{DecisionID: "d1", Features: map[string]float64{"amount_minor": 94000}, Tx: store.TxAttrs{MCC: "5732", Channel: "ECOMMERCE", Country: "US", Currency: "USD"}},
		{DecisionID: "d2", Features: map[string]float64{"amount_minor": 500}, Tx: store.TxAttrs{MCC: "5411", Channel: "POS", Country: "US", Currency: "USD"}},
	}}
	return &CaseServer{Backend: b, Rules: testRules(t), Live: NewLiveStats(nil), ShadowDropped: func() int64 { return 7 }, LiveEvery: 10 * time.Millisecond}, b
}

func analystCtx() context.Context {
	return WithPrincipal(context.Background(), Principal{Subject: "analyst-1", Role: auth.RoleAnalyst})
}

func TestListCasesValidatesInput(t *testing.T) {
	s, b := newCaseServer(t)
	ctx := analystCtx()
	for name, req := range map[string]*fraudv1.ListCasesRequest{
		"page_size":      {PageSize: 101},
		"negative size":  {PageSize: -1},
		"summary_status": {SummaryStatus: "done"},
		"reason_code":    {ReasonCode: "lower case"},
		"status":         {Status: fraudv1.CaseStatus(99)},
	} {
		if _, err := s.ListCases(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := s.ListCases(ctx, &fraudv1.ListCasesRequest{PageToken: "bad"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad page token: %v", err)
	}
	resp, err := s.ListCases(ctx, &fraudv1.ListCasesRequest{Status: fraudv1.CaseStatus_CASE_STATUS_LABELLED, SummaryStatus: "ok"})
	if err != nil || len(resp.Cases) != 1 || resp.NextPageToken != "next" || resp.TotalOpen != 1 {
		t.Fatalf("list %v %v", resp, err)
	}
	if b.lastQ.PageSize != 50 || b.lastQ.Status != "labelled" {
		t.Fatalf("query defaults %+v", b.lastQ)
	}
}

func TestGetCaseMapsNotFound(t *testing.T) {
	s, _ := newCaseServer(t)
	if _, err := s.GetCase(analystCtx(), &fraudv1.GetCaseRequest{CaseId: "not-a-uuid"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad id: %v", err)
	}
	if _, err := s.GetCase(analystCtx(), &fraudv1.GetCaseRequest{CaseId: "0192f000-0000-7000-8000-0000000000bb"}); status.Code(err) != codes.NotFound {
		t.Fatalf("missing: %v", err)
	}
	if c, err := s.GetCase(analystCtx(), &fraudv1.GetCaseRequest{CaseId: testCaseID}); err != nil || c.CaseId != testCaseID {
		t.Fatalf("get %v %v", c, err)
	}
}

func TestLabelCaseValidatesAndRecordsActor(t *testing.T) {
	s, b := newCaseServer(t)
	good := &fraudv1.LabelCaseRequest{CaseId: testCaseID, Label: fraudv1.Label_LABEL_FRAUD, Note: "card testing", IdempotencyKey: "key-00000001"}
	bad := []*fraudv1.LabelCaseRequest{
		{CaseId: "x", Label: fraudv1.Label_LABEL_FRAUD, IdempotencyKey: "key-00000001"},
		{CaseId: testCaseID, Label: fraudv1.Label_LABEL_UNSPECIFIED, IdempotencyKey: "key-00000001"},
		{CaseId: testCaseID, Label: fraudv1.Label_LABEL_FRAUD, IdempotencyKey: "short"},
		{CaseId: testCaseID, Label: fraudv1.Label_LABEL_FRAUD, IdempotencyKey: "key 00000001"},
		{CaseId: testCaseID, Label: fraudv1.Label_LABEL_FRAUD, IdempotencyKey: "key-00000001", Note: string(make([]rune, 2001))},
	}
	for i, req := range bad {
		if _, err := s.LabelCase(analystCtx(), req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("case %d: %v", i, err)
		}
	}
	if _, err := s.LabelCase(context.Background(), good); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no principal: %v", err)
	}
	c, err := s.LabelCase(analystCtx(), good)
	if err != nil || c.Status != fraudv1.CaseStatus_CASE_STATUS_LABELLED {
		t.Fatalf("label %v %v", c, err)
	}
	if len(b.labels) != 1 || b.labels[0].ActorSub != "analyst-1" || b.labels[0].Role != "analyst" || b.labels[0].Label != "fraud" {
		t.Fatalf("recorded %+v", b.labels)
	}
	conflict := &fraudv1.LabelCaseRequest{CaseId: testCaseID, Label: fraudv1.Label_LABEL_LEGIT, IdempotencyKey: "key-00000002"}
	if _, err := s.LabelCase(analystCtx(), conflict); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("conflicting label: %v", err)
	}
}

func TestTimelineAndShadowValidation(t *testing.T) {
	s, _ := newCaseServer(t)
	ctx := analystCtx()
	if _, err := s.GetTimeline(ctx, &fraudv1.GetTimelineRequest{AccountId: "bob"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("account: %v", err)
	}
	if _, err := s.GetTimeline(ctx, &fraudv1.GetTimelineRequest{AccountId: "acc_00000042", Limit: 201}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("limit: %v", err)
	}
	tl, err := s.GetTimeline(ctx, &fraudv1.GetTimelineRequest{AccountId: "acc_00000042"})
	if err != nil || tl.AccountId != "acc_00000042" || len(tl.Items) != 1 {
		t.Fatalf("timeline %v %v", tl, err)
	}
	if _, err := s.GetShadowSummary(ctx, &fraudv1.GetShadowSummaryRequest{WindowHours: 721}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("window: %v", err)
	}
	if _, err := s.GetShadowSummary(ctx, &fraudv1.GetShadowSummaryRequest{DisagreementLimit: 101}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("disagreement limit: %v", err)
	}
	sum, err := s.GetShadowSummary(ctx, &fraudv1.GetShadowSummaryRequest{})
	if err != nil || sum.Total != 3 || sum.Dropped != 7 {
		t.Fatalf("shadow %v %v", sum, err)
	}
}

func TestActiveRuleSetMatchesEngine(t *testing.T) {
	s, _ := newCaseServer(t)
	rs, err := s.GetActiveRuleSet(analystCtx(), &fraudv1.GetActiveRuleSetRequest{})
	if err != nil || rs.Version != "2026.10.2" || len(rs.Rules) == 0 || rs.Sha256 == "" || rs.Thresholds.Decline <= rs.Thresholds.Review {
		t.Fatalf("rule set %v %v", rs, err)
	}
}

func TestTestRuleEvaluatesWithoutActivating(t *testing.T) {
	s, _ := newCaseServer(t)
	ctx := analystCtx()
	before := s.Rules.Active().SHA256
	resp, err := s.TestRule(ctx, &fraudv1.TestRuleRequest{When: "amount_minor > 10000 && mcc == \"5732\""})
	if err != nil || !resp.Valid || resp.Evaluated != 2 || resp.Matched != 1 || resp.MatchRate != 0.5 || resp.SampleDecisionIds[0] != "d1" {
		t.Fatalf("test rule %v %v", resp, err)
	}
	resp, err = s.TestRule(ctx, &fraudv1.TestRuleRequest{When: "amount_minor +"})
	if err != nil || resp.Valid || resp.Error == "" {
		t.Fatalf("syntax error must be reported, not raised: %v %v", resp, err)
	}
	if _, err := s.TestRule(ctx, &fraudv1.TestRuleRequest{When: ""}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty: %v", err)
	}
	// Security audit M6 H1: nested predicates over ranges are rejected at compile time, not evaluated.
	resp, err = s.TestRule(ctx, &fraudv1.TestRuleRequest{When: "all(1..990, {all(1..990, {# > 0 && # < 2000 && # * # > 0})})"})
	if err != nil || resp.Valid || !strings.Contains(resp.Error, "not allowed") {
		t.Fatalf("unbounded expression must be rejected: %v %v", resp, err)
	}
	if _, err := s.TestRule(ctx, &fraudv1.TestRuleRequest{When: "true", SampleSize: 5001}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("sample size: %v", err)
	}
	if s.Rules.Active().SHA256 != before {
		t.Fatal("TestRule must not change the active rule set")
	}
}

func TestLiveStatsSnapshot(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	l := NewLiveStats(clock)
	for i := range 20 {
		d := &fraudv1.Decision{Action: fraudv1.Action_ACTION_APPROVE}
		if i%4 == 0 {
			d = &fraudv1.Decision{Action: fraudv1.Action_ACTION_REVIEW, Degraded: true, ReasonCodes: []string{"MODEL_UNAVAILABLE"}}
		}
		l.Record(d, time.Duration(i+1)*time.Millisecond)
	}
	mu.Lock()
	now = now.Add(time.Second)
	mu.Unlock()
	m := l.Snapshot(10 * time.Second)
	if m.Approve != 15 || m.Review != 5 || m.ModelUnavailable != 5 || m.Rps != 2 || m.DegradedRate != 0.25 || m.P95Ms != 19 {
		t.Fatalf("snapshot %+v", m)
	}
	// Buckets older than the window drop out.
	mu.Lock()
	now = now.Add(time.Minute)
	mu.Unlock()
	if m := l.Snapshot(10 * time.Second); m.Approve != 0 || m.Rps != 0 {
		t.Fatalf("stale snapshot %+v", m)
	}
}

// grpcCaseClient runs the CaseService behind the real auth interceptors and auth.Verifier.
func grpcCaseClient(t *testing.T, s *CaseServer) (fraudv1.CaseServiceClient, *auth.Signer) {
	t.Helper()
	signer, err := auth.NewSigner("http://dev-issuer.test")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := auth.KeysFromJWKS(signer.JWKS())
	if err != nil {
		t.Fatal(err)
	}
	cfg := AuthConfig{ServiceToken: "svc-token", Verifier: JWTVerifier{V: &auth.Verifier{Issuer: "http://dev-issuer.test", Keys: keys}}, Authorize: RBAC}
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer(grpc.UnaryInterceptor(cfg.UnaryAuth()), grpc.StreamInterceptor(cfg.StreamAuth()))
	fraudv1.RegisterCaseServiceServer(gs, s)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	}), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return fraudv1.NewCaseServiceClient(conn), signer
}

func bearerCtx(t *testing.T, signer *auth.Signer, sub, role string) context.Context {
	t.Helper()
	tok, err := signer.Mint(sub, role, sub, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
}

func TestAnalystCannotTestRule(t *testing.T) {
	s, _ := newCaseServer(t)
	client, signer := grpcCaseClient(t, s)
	analyst := bearerCtx(t, signer, "analyst-1", auth.RoleAnalyst)
	risk := bearerCtx(t, signer, "risk-1", auth.RoleRiskEngineer)
	ml := bearerCtx(t, signer, "ml-1", auth.RoleMLEngineer)

	if _, err := client.TestRule(analyst, &fraudv1.TestRuleRequest{When: "true"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("analyst TestRule: %v", err)
	}
	if _, err := client.TestRule(risk, &fraudv1.TestRuleRequest{When: "true"}); err != nil {
		t.Fatalf("risk engineer TestRule: %v", err)
	}
	label := &fraudv1.LabelCaseRequest{CaseId: testCaseID, Label: fraudv1.Label_LABEL_FRAUD, IdempotencyKey: "key-00000001"}
	if _, err := client.LabelCase(risk, label); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("risk engineer LabelCase: %v", err)
	}
	if _, err := client.LabelCase(analyst, label); err != nil {
		t.Fatalf("analyst LabelCase: %v", err)
	}
	if _, err := client.GetShadowSummary(analyst, &fraudv1.GetShadowSummaryRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("analyst shadow: %v", err)
	}
	if _, err := client.GetShadowSummary(ml, &fraudv1.GetShadowSummaryRequest{}); err != nil {
		t.Fatalf("ml engineer shadow: %v", err)
	}
	svc := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer svc-token")
	if _, err := client.ListCases(svc, &fraudv1.ListCasesRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("service token on console: %v", err)
	}
	if _, err := client.ListCases(context.Background(), &fraudv1.ListCasesRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("anonymous: %v", err)
	}
}

func TestStreamLiveMetricsPushesUntilCancelled(t *testing.T) {
	s, _ := newCaseServer(t)
	client, signer := grpcCaseClient(t, s)
	ctx, cancel := context.WithCancel(bearerCtx(t, signer, "analyst-1", auth.RoleAnalyst))
	stream, err := client.StreamLiveMetrics(ctx, &fraudv1.StreamLiveMetricsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		m, err := stream.Recv()
		if err != nil || m.Ts == nil {
			t.Fatalf("recv %v %v", m, err)
		}
	}
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := stream.Recv(); err != nil {
			if status.Code(err) != codes.Canceled {
				t.Fatalf("after cancel: %v", err)
			}
			return
		}
	}
	t.Fatal("stream did not end after cancel")
}

func TestScoreFeedsLiveStats(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	live := NewLiveStats(func() time.Time { return now })
	h.srv.d.Live = live
	if _, err := h.srv.Score(context.Background(), &fraudv1.ScoreRequest{Transaction: journeyTx("0192f000-0000-7000-8000-0000000000c1")}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if m := live.Snapshot(time.Second); m.Review != 1 || m.Rps != 1 {
		t.Fatalf("live %+v", m)
	}
}
