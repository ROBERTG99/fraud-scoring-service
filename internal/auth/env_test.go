package auth

import "testing"

// Security audit: the dev-issuer mints tokens for anyone, and JWKS over plain HTTP can be spoofed;
// both are allowed only when FRAUD_ENV=dev says so.
func TestDevModeAndJWKSURL(t *testing.T) {
	t.Setenv("FRAUD_ENV", "")
	if DevMode() {
		t.Fatal("dev mode without FRAUD_ENV=dev")
	}
	if err := CheckJWKSURL("http://dev-issuer:8515/.well-known/jwks.json"); err == nil {
		t.Fatal("plain-HTTP JWKS accepted outside dev")
	}
	if err := CheckJWKSURL("https://issuer.example/.well-known/jwks.json"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FRAUD_ENV", "dev")
	if !DevMode() || CheckJWKSURL("http://dev-issuer:8515/.well-known/jwks.json") != nil {
		t.Fatal("dev mode must allow the local issuer")
	}
	if err := CheckJWKSURL("ftp://x/jwks"); err == nil {
		t.Fatal("non-HTTP scheme accepted")
	}
}

// Security audit round 2, N8: migrations create every service role with its name as its password; outside
// dev mode a service refuses those credentials, including the default DSNs.
func TestDevDatabaseCredentialsOnlyInDevMode(t *testing.T) {
	t.Setenv("FRAUD_ENV", "")
	t.Setenv("PGPASSWORD", "")
	t.Setenv("PGPASSFILE", t.TempDir()+"/none")
	for _, dsn := range []string{
		"postgres://fraud_app:fraud_app@127.0.0.1:8520/fraud?sslmode=disable",
		"postgresql://fraud_summarizer:fraud_summarizer@db/fraud",
		"postgres://fraud_retention@db/fraud", // no password at all
		"not a url",
	} {
		if err := CheckDatabaseDSN(dsn); err == nil {
			t.Errorf("%q accepted outside dev", dsn)
		}
	}
	if err := CheckDatabaseDSN("postgres://fraud_app:s3cr3t-from-vault@db/fraud?sslmode=verify-full"); err != nil {
		t.Fatal(err)
	}
	// Round-3 audit: the check reads the configuration the driver parses, not the URL text.
	for _, dsn := range []string{
		"postgres://fraud_app:x@db/fraud?password=fraud_app", // a query parameter overrides the userinfo
		"postgres://fraud%5Fapp:fraud%5Fapp@db/fraud",        // percent-encoded development password
		"host=db user=fraud_app password=fraud_app dbname=fraud",
	} {
		if err := CheckDatabaseDSN(dsn); err == nil {
			t.Errorf("%q accepted outside dev", dsn)
		}
	}
	if err := CheckDatabaseDSN("host=db user=fraud_app password=from-a-secret dbname=fraud"); err != nil {
		t.Fatalf("keyword/value DSN with a real password: %v", err)
	}
	t.Setenv("PGPASSWORD", "from-the-environment")
	if err := CheckDatabaseDSN("postgres://fraud_app@db/fraud"); err != nil {
		t.Fatalf("password from PGPASSWORD: %v", err)
	}
	t.Setenv("FRAUD_ENV", "dev")
	if err := CheckDatabaseDSN("postgres://fraud_app:fraud_app@127.0.0.1:8520/fraud"); err != nil {
		t.Fatal(err)
	}
}
