package api

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"runtime/debug"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	fraudv1 "github.com/ROBERTG99/fraud-scoring-service/gen/go/fraud/v1"
)

// Principal is the authenticated caller.
type Principal struct {
	Subject string
	Role    string // service | analyst | risk_engineer | ml_engineer
}

type principalKey struct{}

// WithPrincipal stores the caller in ctx.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the caller stored by the auth interceptor.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// TokenVerifier verifies a console JWT and returns the caller (internal/auth).
type TokenVerifier interface {
	Verify(ctx context.Context, token string) (Principal, error)
}

// Authorizer decides whether a principal may call a full gRPC method name (RBAC, internal/auth).
type Authorizer func(method string, p Principal) error

// AuthConfig configures authentication per service.
type AuthConfig struct {
	ServiceToken string        // Bearer token for fraud.v1.Scorer
	Verifier     TokenVerifier // dev-issuer JWTs for fraud.v1.CaseService
	Authorize    Authorizer
}

func bearer(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	for _, v := range md.Get("authorization") {
		if after, ok := strings.CutPrefix(v, "Bearer "); ok {
			return strings.TrimSpace(after)
		}
	}
	return ""
}

// healthCheck is the one method open without credentials: a load balancer's probe, answered with SERVING.
// Watch (a held stream) and List (every service's status) need a token like any other call (D-059).
const healthCheck = "/grpc.health.v1.Health/Check"

func (c AuthConfig) authenticate(ctx context.Context, method string) (context.Context, error) {
	if method == healthCheck {
		return ctx, nil
	}
	token := bearer(ctx)
	if token == "" {
		return nil, status.Error(codes.Unauthenticated, "missing bearer token")
	}
	if strings.HasPrefix(method, "/"+fraudv1.Scorer_ServiceDesc.ServiceName+"/") {
		if c.ServiceToken == "" || subtle.ConstantTimeCompare([]byte(token), []byte(c.ServiceToken)) != 1 {
			return nil, status.Error(codes.Unauthenticated, "invalid service token")
		}
		return WithPrincipal(ctx, Principal{Subject: "service", Role: "service"}), nil
	}
	if c.Verifier == nil {
		return nil, status.Error(codes.Unauthenticated, "no token verifier configured")
	}
	p, err := c.Verifier.Verify(ctx, token)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "invalid token")
	}
	if c.Authorize != nil {
		if err := c.Authorize(method, p); err != nil {
			return nil, status.Error(codes.PermissionDenied, err.Error())
		}
	}
	return WithPrincipal(ctx, p), nil
}

// UnaryAuth authenticates and authorizes every unary call.
func (c AuthConfig) UnaryAuth() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, err := c.authenticate(ctx, info.FullMethod)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w wrappedStream) Context() context.Context { return w.ctx }

// StreamAuth authenticates and authorizes every streaming call.
func (c AuthConfig) StreamAuth() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, err := c.authenticate(ss.Context(), info.FullMethod)
		if err != nil {
			return err
		}
		return handler(srv, wrappedStream{ServerStream: ss, ctx: ctx})
	}
}

// UnaryRecover turns panics into Internal errors and logs the stack.
func UnaryRecover(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic in handler", "method", info.FullMethod, "panic", r, "stack", string(debug.Stack()))
				err = status.Error(codes.Internal, "internal error")
			}
		}()
		return handler(ctx, req)
	}
}
