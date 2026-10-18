package auth

import (
	"errors"
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

// CheckDatabaseDSN refuses, outside dev mode, a Postgres DSN without a password or with the development
// password the migrations give every service role (its own name; security audit round 2, N8). The defaults
// built into the services are such DSNs, so a deployment that forgets FRAUD_PG_DSN fails closed.
func CheckDatabaseDSN(dsn string) error {
	if DevMode() {
		return nil
	}
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return errors.New("database DSN: expected postgres://user:password@host/db")
	}
	pw, ok := u.User.Password()
	if !ok || pw == "" || pw == u.User.Username() {
		return fmt.Errorf("database DSN for role %s: development credentials are allowed only with FRAUD_ENV=dev", u.User.Username())
	}
	return nil
}
