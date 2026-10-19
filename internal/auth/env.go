package auth

import (
	"fmt"
	"net/url"
	"os"

	"github.com/jackc/pgx/v5/pgconn"
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

// CheckDatabaseDSN refuses, outside dev mode, database credentials without a password (or client
// certificate), or with the development password the migrations give every service role (its own name;
// security audit round 2, N8). It reads the configuration pgx itself parses (URL or keyword/value, query
// parameters, PGPASSWORD, the passfile), so a "?password=" override cannot slip past it (round 3). The
// defaults built into the services are development DSNs, so a deployment that forgets FRAUD_PG_DSN fails closed.
func CheckDatabaseDSN(dsn string) error {
	if DevMode() {
		return nil
	}
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("database DSN: %w", err)
	}
	certAuth := cfg.TLSConfig != nil && len(cfg.TLSConfig.Certificates) > 0
	switch {
	case cfg.Password == "" && !certAuth:
		return fmt.Errorf("database DSN for role %s: no password or client certificate", cfg.User)
	case cfg.Password != "" && cfg.Password == cfg.User:
		return fmt.Errorf("database DSN for role %s: development credentials are allowed only with FRAUD_ENV=dev", cfg.User)
	}
	return nil
}
