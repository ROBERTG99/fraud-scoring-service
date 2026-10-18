package auth

import "testing"

// Security audit M6 M4: the dev-issuer mints tokens for anyone, and JWKS over plain HTTP can be spoofed;
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
