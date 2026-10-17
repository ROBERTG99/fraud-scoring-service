package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
	"github.com/ROBERTG99/fraud-scoring-service/internal/api"
	"github.com/ROBERTG99/fraud-scoring-service/internal/auth"
	"github.com/ROBERTG99/fraud-scoring-service/internal/features"
	"github.com/ROBERTG99/fraud-scoring-service/internal/model"
	"github.com/ROBERTG99/fraud-scoring-service/internal/rules"
	"github.com/ROBERTG99/fraud-scoring-service/internal/store"
)

type fixedFeatures struct{ spec *features.Spec }

func (f fixedFeatures) ScoreTx(context.Context, features.Tx) (features.Vector, features.Raw, error) {
	return features.Vector{}, features.Raw{}, nil
}
func (f fixedFeatures) Spec() *features.Spec { return f.spec }

type downModel struct{}

func (downModel) Predict(context.Context, string, [][]float32) (model.Prediction, error) {
	return model.Prediction{}, model.ErrUnavailable
}

type memStore struct {
	mu sync.Mutex
	m  map[string]*fraudv1.Decision
}

func (s *memStore) Get(_ context.Context, id string) (*fraudv1.Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[id], nil
}
func (s *memStore) Put(context.Context, *fraudv1.Decision) error { return nil }
func (s *memStore) Commit(_ context.Context, rec store.DecisionRecord) (*fraudv1.Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.m[rec.Decision.TransactionId]; ok {
		return d, nil
	}
	s.m[rec.Decision.TransactionId] = proto.Clone(rec.Decision).(*fraudv1.Decision)
	return rec.Decision, nil
}
func (s *memStore) GetByTransaction(ctx context.Context, id string) (*fraudv1.Decision, error) {
	d, _ := s.Get(ctx, id)
	if d == nil {
		return nil, store.ErrNotFound
	}
	return d, nil
}

type testStack struct {
	base   string
	signer *auth.Signer
}

// startStack runs the real gRPC server, interceptors and HTTP gateway with in-memory dependencies.
func startStack(t *testing.T) testStack {
	t.Helper()
	spec, err := features.LoadSpec("../../features/spec.yaml")
	if err != nil {
		t.Fatal(err)
	}
	eng, err := rules.NewEngine("../../rules", nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewSigner("http://issuer.test")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := auth.KeysFromJWKS(signer.JWKS())
	if err != nil {
		t.Fatal(err)
	}
	mem := &memStore{m: map[string]*fraudv1.Decision{}}
	log := slog.New(slog.DiscardHandler)
	live := api.NewLiveStats(nil)
	scorer := api.NewScorerServer(api.ScorerDeps{Features: fixedFeatures{spec}, Model: downModel{}, Rules: eng, Idempotency: mem, Decisions: mem,
		Live: live, Log: log})
	ctx, cancel := context.WithCancel(context.Background())
	srv, err := newServers(ctx, serverConfig{
		GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0",
		Auth: api.AuthConfig{ServiceToken: "svc-token", Verifier: api.JWTVerifier{V: &auth.Verifier{Issuer: "http://issuer.test", Keys: keys}},
			Authorize: api.RBAC},
		Limiter: api.NewRateLimiter(20, nil),
		Scorer:  scorer,
		Cases:   &api.CaseServer{Rules: eng, Live: live, LiveEvery: 20 * time.Millisecond},
		Log:     log,
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	return testStack{base: "http://" + srv.httpLis.Addr().String(), signer: signer}
}

func do(t *testing.T, method, url, token string, body []byte) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

const journeyBody = `{"transaction":{"transaction_id":"0192f000-0000-7000-8000-000000000001","account_id":"acc_00000042","amount_minor":94000,` +
	`"currency":"USD","merchant_id":"m_00042","mcc":"5732","channel":"CHANNEL_ECOMMERCE","country":"US","ip_prefix":"203.0.113.0/24",` +
	`"device_id":"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789","event_ts":"2026-09-20T10:03:00Z"}}`

func TestScoreReturnsRulesOnlyDecision(t *testing.T) {
	st := startStack(t)
	resp := do(t, http.MethodPost, st.base+"/v1/score", "svc-token", []byte(journeyBody))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var d struct {
		Action       string   `json:"action"`
		ModelVersion string   `json:"model_version"`
		ReasonCodes  []string `json:"reason_codes"`
		Degraded     bool     `json:"degraded"`
		DecisionID   string   `json:"decision_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatal(err)
	}
	if !d.Degraded || d.DecisionID == "" {
		t.Fatalf("expected a degraded rules-only decision, got %+v", d)
	}
	if d.Action != "ACTION_REVIEW" {
		t.Fatalf("amount above the degraded review floor must be REVIEW, got %s", d.Action)
	}
	found := false
	for _, c := range d.ReasonCodes {
		found = found || c == "MODEL_UNAVAILABLE"
	}
	if !found {
		t.Fatalf("expected MODEL_UNAVAILABLE reason, got %v", d.ReasonCodes)
	}
	if resp := do(t, http.MethodGet, st.base+"/healthz", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz %d", resp.StatusCode)
	}
	if resp := do(t, http.MethodGet, st.base+"/metrics", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics %d", resp.StatusCode)
	}
}

func TestGatewayAuthAndLimits(t *testing.T) {
	st := startStack(t)
	if resp := do(t, http.MethodPost, st.base+"/v1/score", "", []byte(journeyBody)); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous score: %d", resp.StatusCode)
	}
	big := `{"transaction":{"merchant_name":"` + strings.Repeat("x", 70<<10) + `"}}`
	if resp := do(t, http.MethodPost, st.base+"/v1/score", "svc-token", []byte(big)); resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Fatalf("oversized body must be a 4xx, got %d", resp.StatusCode)
	}
	if resp := do(t, http.MethodPost, st.base+"/v1/score", "svc-token", []byte(`{"transaction":{"amount_minor":-1}}`)); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid transaction: %d", resp.StatusCode)
	}
	analyst, err := st.signer.Mint("analyst-1", auth.RoleAnalyst, "Analyst One", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if resp := do(t, http.MethodPost, st.base+"/v1/rules:test", analyst, []byte(`{"when":"true"}`)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("analyst TestRule over HTTP: %d", resp.StatusCode)
	}
	resp := do(t, http.MethodGet, st.base+"/v1/rules/active", analyst, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("active rules: %d", resp.StatusCode)
	}
	var rs struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rs); err != nil || rs.Version != "2026.10.2" {
		t.Fatalf("rule set %+v %v", rs, err)
	}
	// 20 req/s per user: a burst beyond that is rejected with 429.
	limited := false
	for range 25 {
		if r := do(t, http.MethodGet, st.base+"/v1/rules/active", analyst, nil); r.StatusCode == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("expected 429 after exceeding the per-user rate limit")
	}
}

func TestLiveMetricsStreamOverGateway(t *testing.T) {
	st := startStack(t)
	ml, err := st.signer.Mint("ml-1", auth.RoleMLEngineer, "ML One", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	resp := do(t, http.MethodGet, st.base+"/v1/metrics/live", ml, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	for i := range 2 {
		if !sc.Scan() {
			t.Fatalf("stream ended after %d messages: %v", i, sc.Err())
		}
		var msg struct {
			Result struct {
				Ts string `json:"ts"`
			} `json:"result"`
		}
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil || msg.Result.Ts == "" {
			t.Fatalf("message %q: %v", sc.Text(), err)
		}
	}
}

func TestRoutesFromProtoAnnotations(t *testing.T) {
	r := httpRoutes()
	cases := map[string]string{
		"/v1/score": "POST",
		"/v1/cases": "GET",
		"/v1/cases/0192f000-0000-7000-8000-0000000000aa":       "GET",
		"/v1/cases/0192f000-0000-7000-8000-0000000000aa:label": "POST",
		"/v1/accounts/acc_000001/timeline":                     "GET",
		"/v1/rules:test":                                       "POST",
		"/v1/nope":                                             "",
	}
	for path, want := range cases {
		if got := allowedMethods(r, path); got != want {
			t.Errorf("%s: %q want %q", path, got, want)
		}
	}
}

func TestWrongMethodIs405WithAllow(t *testing.T) {
	st := startStack(t)
	resp := do(t, http.MethodDelete, st.base+"/v1/score", "svc-token", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "POST" {
		t.Fatalf("status %d allow %q", resp.StatusCode, resp.Header.Get("Allow"))
	}
	if resp := do(t, http.MethodGet, st.base+"/v1/cases?status=1", "svc-token", nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("numeric enum: %d", resp.StatusCode)
	}
}
