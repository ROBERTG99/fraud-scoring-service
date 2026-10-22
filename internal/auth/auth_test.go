package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestAnalystCannotTestRule(t *testing.T) {
	if err := Authorize("/fraud.v1.CaseService/TestRule", RoleAnalyst); err == nil {
		t.Fatal("analyst must not call TestRule")
	}
	if err := Authorize("/fraud.v1.CaseService/TestRule", RoleRiskEngineer); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		method string
		role   string
		ok     bool
	}{
		{"LabelCase", RoleAnalyst, true}, {"LabelCase", RoleRiskEngineer, false}, {"LabelCase", RoleMLEngineer, false},
		{"GetShadowSummary", RoleAnalyst, false}, {"GetShadowSummary", RoleRiskEngineer, true}, {"GetShadowSummary", RoleMLEngineer, true},
		{"ListCases", RoleMLEngineer, true}, {"GetCase", RoleRiskEngineer, true}, {"GetActiveRuleSet", RoleAnalyst, true},
		{"StreamLiveMetrics", RoleAnalyst, true}, {"GetTimeline", RoleAnalyst, true}, {"ListCases", RoleService, false},
		{"DeleteEverything", RoleRiskEngineer, false},
	}
	for _, c := range cases {
		err := Authorize("/fraud.v1.CaseService/"+c.method, c.role)
		if (err == nil) != c.ok {
			t.Errorf("%s as %s: err=%v want ok=%v", c.method, c.role, err, c.ok)
		}
	}
}

func TestMintAndVerify(t *testing.T) {
	s, err := NewSigner("http://dev-issuer:8515")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := KeysFromJWKS(s.JWKS())
	if err != nil {
		t.Fatal(err)
	}
	v := &Verifier{Issuer: s.Issuer, Keys: keys}
	tok, err := s.Mint("analyst-1", RoleAnalyst, "Analyst One", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	id, err := v.Verify(context.Background(), tok)
	if err != nil || id.Subject != "analyst-1" || id.Role != RoleAnalyst {
		t.Fatalf("%+v %v", id, err)
	}
	// Expired, wrong issuer, wrong role, foreign key, alg=none.
	expired, _ := s.Mint("analyst-1", RoleAnalyst, "", -time.Minute)
	wrongRole, _ := s.Mint("x", "admin", "", time.Hour)
	other, _ := NewSigner(s.Issuer)
	foreign, _ := other.Mint("analyst-1", RoleAnalyst, "", time.Hour)
	none := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"sub": "analyst-1", "role": "analyst", "iss": s.Issuer, "aud": Audience, "exp": time.Now().Add(time.Hour).Unix()})
	noneTok, _ := none.SignedString(jwt.UnsafeAllowNoneSignatureType)
	for name, bad := range map[string]string{"expired": expired, "role": wrongRole, "foreign": foreign, "none": noneTok, "garbage": "a.b.c"} {
		if _, err := v.Verify(context.Background(), bad); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s token accepted: %v", name, err)
		}
	}
	other.Issuer = "http://evil"
	wrongIss, _ := other.Mint("a", RoleAnalyst, "", time.Hour)
	otherKeys, _ := KeysFromJWKS(other.JWKS())
	if _, err := (&Verifier{Issuer: s.Issuer, Keys: otherKeys}).Verify(context.Background(), wrongIss); err == nil {
		t.Error("wrong issuer accepted")
	}
}

func TestRemoteKeysFetchAndRefresh(t *testing.T) {
	s, _ := NewSigner("iss")
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(s.JWKS())
	}))
	defer srv.Close()
	rk := &RemoteKeys{URL: srv.URL}
	v := &Verifier{Issuer: "iss", Keys: rk}
	tok, _ := s.Mint("risk-1", RoleRiskEngineer, "", time.Hour)
	for i := 0; i < 3; i++ {
		if _, err := v.Verify(context.Background(), tok); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("JWKS fetched %d times, want 1 (cached)", calls)
	}
	if _, err := rk.Key(context.Background(), "unknown"); err == nil || !strings.Contains(err.Error(), "unknown kid") {
		t.Fatalf("unknown kid: %v", err)
	}
}

type jwksServer struct {
	mu    sync.Mutex
	calls int
	fail  bool
	gate  chan struct{} // when set, each request waits for it
	doc   JWKS
}

func (j *jwksServer) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	j.mu.Lock()
	j.calls++
	fail, gate := j.fail, j.gate
	j.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if fail {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	_ = json.NewEncoder(w).Encode(j.doc)
}

func (j *jwksServer) set(f func(*jwksServer)) { j.mu.Lock(); f(j); j.mu.Unlock() }

func (j *jwksServer) n() int { j.mu.Lock(); defer j.mu.Unlock(); return j.calls }

// Security audit: one fetch for many concurrent misses, none while backing off after a failure, and stale keys
// are served while the issuer is down, but not forever.
func TestRemoteKeysSingleFlightBackoffAndStaleCap(t *testing.T) {
	s, _ := NewSigner("iss")
	js := &jwksServer{doc: s.JWKS(), gate: make(chan struct{})}
	srv := httptest.NewServer(js)
	defer srv.Close()
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	var clock sync.Mutex
	rk := &RemoteKeys{URL: srv.URL, Now: func() time.Time { clock.Lock(); defer clock.Unlock(); return now }}
	advance := func(d time.Duration) { clock.Lock(); now = now.Add(d); clock.Unlock() }
	kid := s.JWKS().Keys[0].Kid
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := rk.Key(ctx, kid); errs <- err }()
	}
	deadline := time.Now().Add(5 * time.Second)
	for js.n() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond) // poll with a deadline
	}
	close(js.gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if js.n() != 1 {
		t.Fatalf("20 concurrent misses made %d fetches, want 1", js.n())
	}

	js.set(func(j *jwksServer) { j.fail, j.gate = true, nil })
	advance(11 * time.Minute) // past fresh: refresh, which fails; the stale key is still served
	if _, err := rk.Key(ctx, kid); err != nil {
		t.Fatalf("stale key while the issuer is down: %v", err)
	}
	if _, err := rk.Key(ctx, kid); err != nil || js.n() != 2 {
		t.Fatalf("backoff: %d fetches (want 2) %v", js.n(), err)
	}
	if _, err := rk.Key(ctx, "unknown"); err == nil || js.n() != 2 {
		t.Fatalf("an unknown kid during backoff must fail without a fetch: %d %v", js.n(), err)
	}
	advance(50 * time.Minute) // 61 min since the last good fetch
	if _, err := rk.Key(ctx, kid); err == nil || !strings.Contains(err.Error(), "jwks") {
		t.Fatalf("keys older than the stale cap must not be used: %v", err)
	}
	js.set(func(j *jwksServer) { j.fail = false })
	advance(11 * time.Second)
	if _, err := rk.Key(ctx, kid); err != nil {
		t.Fatalf("recovers once the issuer answers: %v", err)
	}
}

// A slow refresh does not hold the lock: a known key is served while another caller waits on the issuer.
func TestRemoteKeysServeStaleKeysDuringARefresh(t *testing.T) {
	s, _ := NewSigner("iss")
	js := &jwksServer{doc: s.JWKS()}
	srv := httptest.NewServer(js)
	defer srv.Close()
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	var clock sync.Mutex
	rk := &RemoteKeys{URL: srv.URL, Now: func() time.Time { clock.Lock(); defer clock.Unlock(); return now }}
	kid := s.JWKS().Keys[0].Kid
	if _, err := rk.Key(context.Background(), kid); err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	js.set(func(j *jwksServer) { j.gate = gate })
	clock.Lock()
	now = now.Add(11 * time.Minute)
	clock.Unlock()
	go func() { _, _ = rk.Key(context.Background(), kid) }() // starts the slow refresh
	deadline := time.Now().Add(5 * time.Second)
	for js.n() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond) // poll with a deadline
	}
	got := make(chan error, 1)
	go func() { _, err := rk.Key(context.Background(), kid); got <- err }()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("a known key waited behind the refresh")
	}
	close(gate)
}
