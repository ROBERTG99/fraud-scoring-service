package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
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

func (s *memStore) Get(_ context.Context, id string) (*fraudv1.Decision, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[id], nil, nil
}
func (s *memStore) Put(context.Context, *fraudv1.Decision, []byte) error { return nil }
func (s *memStore) Commit(_ context.Context, rec store.DecisionRecord) (*fraudv1.Decision, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.m[rec.Decision.TransactionId]; ok {
		return d, nil, nil
	}
	s.m[rec.Decision.TransactionId] = proto.Clone(rec.Decision).(*fraudv1.Decision)
	return rec.Decision, rec.RequestHash, nil
}
func (s *memStore) GetByTransaction(ctx context.Context, id string) (*fraudv1.Decision, []byte, error) {
	d, _, _ := s.Get(ctx, id)
	if d == nil {
		return nil, nil, store.ErrNotFound
	}
	return d, nil, nil
}

type testStack struct {
	base     string
	admin    string
	grpcAddr string
	signer   *auth.Signer
	ready    *error // what cfg.Ready returns
}

// startStack runs the real gRPC server, interceptors and HTTP gateway with in-memory dependencies.
func startStack(t *testing.T) testStack {
	t.Helper()
	return startTracedStack(t, nil)
}

func startTracedStack(t *testing.T, tp trace.TracerProvider) testStack {
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
	deps := api.ScorerDeps{Features: fixedFeatures{spec}, Model: downModel{}, Rules: eng, Idempotency: mem, Decisions: mem,
		Live: live, Log: log, Now: func() time.Time { return time.Date(2026, 9, 20, 10, 5, 0, 0, time.UTC) }}
	if tp != nil {
		deps.Tracer = tp.Tracer("test")
	}
	scorer := api.NewScorerServer(deps)
	ctx, cancel := context.WithCancel(context.Background())
	var notReady error
	srv, err := newServers(ctx, serverConfig{
		GRPCAddr: "127.0.0.1:0", HTTPAddr: "127.0.0.1:0", AdminAddr: "127.0.0.1:0",
		Ready: func(context.Context) error { return notReady },
		Auth: api.AuthConfig{ServiceToken: "svc-token", Verifier: api.JWTVerifier{V: &auth.Verifier{Issuer: "http://issuer.test", Keys: keys}},
			Authorize: api.RBAC},
		Limiter:        api.NewRateLimiter(20, nil),
		Scorer:         scorer,
		Cases:          &api.CaseServer{Rules: eng, Live: live, LiveEvery: 20 * time.Millisecond},
		Log:            log,
		TracerProvider: tp,
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
	return testStack{base: "http://" + srv.httpLis.Addr().String(), admin: "http://" + srv.adminLis.Addr().String(),
		grpcAddr: srv.grpcLis.Addr().String(), signer: signer, ready: &notReady}
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
	// Metrics are on the admin listener only, not on the public Score port (security audit).
	if resp := do(t, http.MethodGet, st.base+"/metrics", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("public /metrics %d, want 401 (no such route without a token)", resp.StatusCode)
	}
	if resp := do(t, http.MethodGet, st.admin+"/metrics", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("admin metrics %d", resp.StatusCode)
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

func TestScoreContinuesTheCallersTrace(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	st := startTracedStack(t, tp)
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, st.base+"/v1/score", strings.NewReader(journeyBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer svc-token")
	req.Header.Set("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	// A real gRPC health check (no token needed, D-052) must not produce a span: the filter is exercised.
	conn, err := grpc.NewClient(st.grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatal(err)
	}
	// grpc-go ends the server span (the stats handler's End event) after it has written the status, so the
	// gateway can answer before the span is recorded (seen on CI at cb95269): wait for it.
	spans := map[string]sdktrace.ReadOnlySpan{}
	var server sdktrace.ReadOnlySpan
	for wait := time.Now().Add(2 * time.Second); ; {
		for _, s := range rec.Ended() {
			spans[s.Name()] = s
		}
		var ok bool
		if server, ok = spans["fraud.v1.Scorer/Score"]; ok {
			break
		}
		if time.Now().After(wait) {
			t.Fatalf("no gRPC server span among %v", spans)
		}
		time.Sleep(5 * time.Millisecond)
	}
	for name := range spans {
		if strings.Contains(name, "grpc.health") {
			t.Fatalf("health checks are traced: %s", name)
		}
	}
	if server.SpanKind() != trace.SpanKindServer || server.SpanContext().TraceID().String() != traceID {
		t.Fatalf("server span kind %v trace %s, want the caller's trace %s", server.SpanKind(), server.SpanContext().TraceID(), traceID)
	}
	if score, ok := spans["fraud.score"]; !ok || score.Parent().SpanID() != server.SpanContext().SpanID() {
		t.Fatal("fraud.score is not a child of the gRPC server span")
	}
}

// TestBodyOver64KBRejected: cap request bodies at 64 KB on both surfaces.
func TestBodyOver64KBRejected(t *testing.T) {
	st := startStack(t)
	pad := func(total int) []byte { // a JSON body of exactly total bytes
		head, tail := `{"transaction":{"merchant_name":"`, `"}}`
		return []byte(head + strings.Repeat("x", total-len(head)-len(tail)) + tail)
	}
	at := do(t, http.MethodPost, st.base+"/v1/score", "svc-token", pad(api.MaxBodyBytes))
	if at.StatusCode == http.StatusRequestEntityTooLarge {
		t.Fatal("a body of exactly 64 KB must be read (then fail validation), not rejected for size")
	}
	over := do(t, http.MethodPost, st.base+"/v1/score", "svc-token", pad(api.MaxBodyBytes+1))
	var body struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(over.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if over.StatusCode != http.StatusRequestEntityTooLarge || body.Code != int(codes.ResourceExhausted) {
		t.Fatalf("64 KB + 1 byte: status %d code %d, want 413 RESOURCE_EXHAUSTED", over.StatusCode, body.Code)
	}

	conn, err := grpc.NewClient(st.grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(1<<20)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer svc-token"), 5*time.Second)
	defer cancel()
	big := &fraudv1.ScoreRequest{Transaction: &fraudv1.Transaction{MerchantName: strings.Repeat("x", api.MaxBodyBytes)}}
	if _, err := fraudv1.NewScorerClient(conn).Score(ctx, big); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("gRPC message over 64 KB: %v, want ResourceExhausted", err)
	}
}

// Metadata is bounded like HTTP headers: 8 MB of it used to reach the auth check (security audit).
func TestGRPCMetadataOverHeaderLimitRejected(t *testing.T) {
	st := startStack(t)
	conn, err := grpc.NewClient(st.grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	call := func(pad int) error {
		ctx, cancel := context.WithTimeout(metadata.AppendToOutgoingContext(context.Background(), "x-pad", strings.Repeat("x", pad)), 5*time.Second)
		defer cancel()
		_, err := fraudv1.NewScorerClient(conn).Score(ctx, &fraudv1.ScoreRequest{})
		return err
	}
	if err := call(1 << 10); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("small metadata, no token: %v, want Unauthenticated", err)
	}
	if err := call(api.MaxHeaderBytes + 1); err == nil || status.Code(err) == codes.Unauthenticated {
		t.Fatalf("metadata over %d bytes reached the auth check: %v", api.MaxHeaderBytes, err)
	}
}

// The public /readyz says only "not ready"; the cause (hostnames, driver errors) is on the admin listener.
func TestReadyzDetailOnlyOnAdmin(t *testing.T) {
	st := startStack(t)
	*st.ready = errors.New("postgres: dial tcp 10.0.0.7:5432: connection refused")
	pub := do(t, http.MethodGet, st.base+"/readyz", "", nil)
	body, _ := io.ReadAll(pub.Body)
	if pub.StatusCode != http.StatusServiceUnavailable || strings.Contains(string(body), "10.0.0.7") {
		t.Fatalf("public readyz %d %q", pub.StatusCode, body)
	}
	adm := do(t, http.MethodGet, st.admin+"/readyz", "", nil)
	body, _ = io.ReadAll(adm.Body)
	if adm.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "connection refused") {
		t.Fatalf("admin readyz %d %q", adm.StatusCode, body)
	}
}

// gRPC health checks need no token (security audit): load balancers probe without credentials.
func TestGRPCHealthNeedsNoToken(t *testing.T) {
	st := startStack(t)
	conn, err := grpc.NewClient(st.grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil || resp.Status != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("health: %v %v", resp, err)
	}
	// Only Check is open: Watch holds a stream and List enumerates services (security audit round 2, N7).
	w, err := healthpb.NewHealthClient(conn).Watch(ctx, &healthpb.HealthCheckRequest{})
	if err == nil {
		_, err = w.Recv()
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("anonymous Watch: %v, want Unauthenticated", err)
	}
	if _, err := healthpb.NewHealthClient(conn).List(ctx, &healthpb.HealthListRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("anonymous List: %v, want Unauthenticated", err)
	}
}

// rawGRPCStatus sends one gRPC request with an extra metadata header of pad bytes over a hand-written HTTP/2
// connection (no client-side size check) and returns the grpc-status the server answers, or "" when it
// answers none (the stream or connection is refused before the handler).
func rawGRPCStatus(t *testing.T, addr string, pad int) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte(http2.ClientPreface)); err != nil {
		t.Fatal(err)
	}
	fr := http2.NewFramer(conn, conn)
	if err := fr.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	var block bytes.Buffer
	enc := hpack.NewEncoder(&block)
	for _, f := range [][2]string{{":method", "POST"}, {":scheme", "http"}, {":path", "/fraud.v1.Scorer/Score"},
		{":authority", addr}, {"content-type", "application/grpc"}, {"te", "trailers"}, {"x-pad", strings.Repeat("x", pad)}} {
		_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
	}
	b := block.Bytes()
	const chunk = 16_000 // under the default 16 KB frame size: the rest goes in CONTINUATION frames
	first := b[:min(chunk, len(b))]
	if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: first, EndHeaders: len(b) <= chunk}); err != nil {
		return ""
	}
	for off := chunk; off < len(b); off += chunk {
		if err := fr.WriteContinuation(1, off+chunk >= len(b), b[off:min(off+chunk, len(b))]); err != nil {
			return ""
		}
	}
	// One empty gRPC message (uncompressed, length 0): the server reads it before the interceptors run.
	if err := fr.WriteData(1, true, []byte{0, 0, 0, 0, 0}); err != nil {
		return ""
	}
	dec := hpack.NewDecoder(4096, nil)
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			return "" // connection closed
		}
		switch f := f.(type) {
		case *http2.HeadersFrame:
			fields, _ := dec.DecodeFull(f.HeaderBlockFragment())
			for _, h := range fields {
				if h.Name == "grpc-status" {
					return h.Value
				}
			}
		case *http2.RSTStreamFrame, *http2.GoAwayFrame:
			return ""
		case *http2.SettingsFrame:
			if !f.IsAck() {
				_ = fr.WriteSettingsAck()
			}
		}
	}
}

// The server enforces the metadata limit itself (security audit round 2): a client that ignores the
// advertised limit never reaches the auth check.
func TestServerRejectsOversizedMetadataBeforeAuth(t *testing.T) {
	st := startStack(t)
	if got := rawGRPCStatus(t, st.grpcAddr, 1<<10); got != "16" {
		t.Fatalf("1 KB of metadata without a token: grpc-status %q, want 16 (Unauthenticated)", got)
	}
	for _, pad := range []int{api.MaxHeaderBytes + 1, 900 << 10} {
		if got := rawGRPCStatus(t, st.grpcAddr, pad); got == "16" {
			t.Fatalf("%d bytes of metadata reached the auth check", pad)
		}
	}
}
