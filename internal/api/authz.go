package api

import (
	"context"

	"github.com/ROBERTG99/fraud-scoring-service/internal/auth"
)

// JWTVerifier adapts auth.Verifier to TokenVerifier.
type JWTVerifier struct{ V *auth.Verifier }

// Verify validates a dev-issuer token.
func (j JWTVerifier) Verify(ctx context.Context, token string) (Principal, error) {
	id, err := j.V.Verify(ctx, token)
	if err != nil {
		return Principal{}, err
	}
	return Principal{Subject: id.Subject, Role: id.Role}, nil
}

// RBAC enforces the auth.MethodRoles table.
func RBAC(method string, p Principal) error { return auth.Authorize(method, p.Role) }
