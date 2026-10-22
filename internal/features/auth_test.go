package features

import "testing"

// TestRedisClientsUseConfiguredCredentials: security audit. Deployments run Redis with AUTH (an ACL
// user per service); every client takes its credentials from the environment.
func TestRedisClientsUseConfiguredCredentials(t *testing.T) {
	t.Setenv("FRAUD_REDIS_USERNAME", "scorer")
	t.Setenv("FRAUD_REDIS_PASSWORD", "s3cret")
	for _, c := range []interface{ Close() error }{NewRedisClient("127.0.0.1:1"), NewUpdaterRedisClient("127.0.0.1:1")} {
		_ = c.Close()
	}
	if o := NewRedisClient("127.0.0.1:1").Options(); o.Username != "scorer" || o.Password != "s3cret" {
		t.Fatalf("scoring client credentials %q/%q", o.Username, o.Password)
	}
	if o := NewUpdaterRedisClient("127.0.0.1:1").Options(); o.Username != "scorer" || o.Password != "s3cret" {
		t.Fatalf("updater client credentials %q/%q", o.Username, o.Password)
	}
	if u, p := RedisCredentials(); u != "scorer" || p != "s3cret" {
		t.Fatalf("RedisCredentials %q/%q", u, p)
	}
}
