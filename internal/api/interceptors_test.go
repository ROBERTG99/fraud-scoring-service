package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type staticVerifier map[string]Principal

func (s staticVerifier) Verify(_ context.Context, token string) (Principal, error) {
	if p, ok := s[token]; ok {
		return p, nil
	}
	return Principal{}, errors.New("bad token")
}

func call(t *testing.T, cfg AuthConfig, method, token string) (Principal, error) {
	t.Helper()
	ctx := context.Background()
	if token != "" {
		ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", "Bearer "+token))
	}
	var got Principal
	_, err := cfg.UnaryAuth()(ctx, nil, &grpc.UnaryServerInfo{FullMethod: method}, func(ctx context.Context, _ any) (any, error) {
		got, _ = PrincipalFrom(ctx)
		return nil, nil
	})
	return got, err
}

func TestScorerRequiresServiceToken(t *testing.T) {
	cfg := AuthConfig{ServiceToken: "s3cret", Verifier: staticVerifier{"jwt-analyst": {Subject: "analyst-1", Role: "analyst"}}}
	if _, err := call(t, cfg, "/fraud.v1.Scorer/Score", ""); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("missing token: %v", err)
	}
	if _, err := call(t, cfg, "/fraud.v1.Scorer/Score", "wrong"); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("wrong token: %v", err)
	}
	// A valid console JWT is not a service token.
	if _, err := call(t, cfg, "/fraud.v1.Scorer/Score", "jwt-analyst"); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("jwt on Score: %v", err)
	}
	p, err := call(t, cfg, "/fraud.v1.Scorer/Score", "s3cret")
	if err != nil || p.Role != "service" {
		t.Fatalf("service token: %v %+v", err, p)
	}
}

func TestCaseServiceRequiresJWTAndRBAC(t *testing.T) {
	cfg := AuthConfig{ServiceToken: "s3cret", Verifier: staticVerifier{"jwt-analyst": {Subject: "analyst-1", Role: "analyst"}},
		Authorize: func(method string, p Principal) error {
			if method == "/fraud.v1.CaseService/TestRule" && p.Role != "risk_engineer" {
				return errors.New("role analyst may not call TestRule")
			}
			return nil
		}}
	if _, err := call(t, cfg, "/fraud.v1.CaseService/ListCases", "s3cret"); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("service token on console API: %v", err)
	}
	p, err := call(t, cfg, "/fraud.v1.CaseService/ListCases", "jwt-analyst")
	if err != nil || p.Subject != "analyst-1" {
		t.Fatalf("analyst list: %v", err)
	}
	if _, err := call(t, cfg, "/fraud.v1.CaseService/TestRule", "jwt-analyst"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("analyst TestRule: %v", err)
	}
	if _, err := call(t, AuthConfig{}, "/fraud.v1.CaseService/ListCases", "x"); status.Code(err) != codes.Unauthenticated {
		t.Fatal("no verifier must reject")
	}
}

func TestUnaryRecover(t *testing.T) {
	_, err := UnaryRecover(slog.New(slog.NewTextHandler(io.Discard, nil)))(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/x"},
		func(context.Context, any) (any, error) { panic("boom") })
	if status.Code(err) != codes.Internal {
		t.Fatalf("panic not recovered: %v", err)
	}
}
