package auth

import (
	"fmt"
	"net/url"
	"os"
)

// DevMode reports FRAUD_ENV=dev: the local stacks (compose, seed_host.sh, tests) where the dev-issuer may
// mint tokens and JWKS may be fetched over plain HTTP. Anything else is treated as a deployment (audit M6 M4).
func DevMode() bool { return os.Getenv("FRAUD_ENV") == "dev" }

// CheckJWKSURL requires HTTPS for the JWKS endpoint outside dev mode: keys fetched over plain HTTP could be
// replaced in transit, and with them every console token.
func CheckJWKSURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("JWKS URL: %w", err)
	}
	switch {
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && DevMode():
		return nil
	case u.Scheme == "http":
		return fmt.Errorf("JWKS URL %s: plain HTTP is allowed only with FRAUD_ENV=dev", raw)
	default:
		return fmt.Errorf("JWKS URL %s: scheme must be https", raw)
	}
}
