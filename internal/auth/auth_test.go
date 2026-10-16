package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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
