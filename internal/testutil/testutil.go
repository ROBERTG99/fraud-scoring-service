// Package testutil provides Postgres and Redis for integration tests.
//
// Resolution order: FRAUD_TEST_PG_DSN / FRAUD_TEST_REDIS_ADDR (set by `make test` from compose.test.yaml),
// then a local redis-server binary (Redis only), then testcontainers-go. Each test gets an isolated
// database or key prefix, so tests can run in parallel without retries.
package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"
)

// UniqueName returns prefix plus 12 random hex characters.
func UniqueName(prefix string) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

var (
	pgOnce  sync.Once
	pgAdmin string
	pgErr   error
)

// PostgresAdminDSN returns a superuser DSN for a Postgres 16 server.
func PostgresAdminDSN(t testing.TB) string {
	t.Helper()
	pgOnce.Do(func() {
		if dsn := os.Getenv("FRAUD_TEST_PG_DSN"); dsn != "" {
			pgAdmin = dsn
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		c, err := tcpostgres.Run(ctx, "postgres:16-alpine",
			tcpostgres.WithDatabase("fraud"), tcpostgres.WithUsername("fraud"), tcpostgres.WithPassword("fraud"),
			testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(2*time.Minute)))
		if err != nil {
			pgErr = err
			return
		}
		pgAdmin, pgErr = c.ConnectionString(ctx, "sslmode=disable")
	})
	if pgErr != nil {
		t.Fatalf("postgres unavailable (set FRAUD_TEST_PG_DSN or run Docker): %v", pgErr)
	}
	return pgAdmin
}

// NewDatabase creates an empty database and returns its admin DSN; it is dropped when the test ends.
func NewDatabase(t testing.TB) string {
	t.Helper()
	admin := PostgresAdminDSN(t)
	name := UniqueName("t_")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := pgx.Connect(ctx, admin)
		if err != nil {
			return
		}
		defer func() { _ = c.Close(ctx) }()
		_, _ = c.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

// WithUser returns dsn with different credentials (for role-permission tests).
func WithUser(t testing.TB, dsn, user, password string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(user, password)
	return u.String()
}

var (
	redisOnce sync.Once
	redisAddr string
	redisErr  error
)

// RedisAddr returns the address of a Redis 7 server.
func RedisAddr(t testing.TB) string {
	t.Helper()
	redisOnce.Do(func() {
		if addr := os.Getenv("FRAUD_TEST_REDIS_ADDR"); addr != "" {
			redisAddr = addr
			return
		}
		if bin, err := exec.LookPath("redis-server"); err == nil {
			redisAddr, redisErr = startLocalRedis(bin)
			if redisErr == nil {
				return
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		c, err := tcredis.Run(ctx, "redis:7-alpine")
		if err != nil {
			redisErr = err
			return
		}
		ep, err := c.Endpoint(ctx, "")
		redisAddr, redisErr = ep, err
	})
	if redisErr != nil {
		t.Fatalf("redis unavailable (set FRAUD_TEST_REDIS_ADDR or install redis-server): %v", redisErr)
	}
	return redisAddr
}

func startLocalRedis(bin string) (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	dir, err := os.MkdirTemp("", "redis-test-")
	if err != nil {
		return "", err
	}
	cmd := exec.Command(bin, "--port", strconv.Itoa(port), "--bind", "127.0.0.1", "--save", "", "--appendonly", "no", "--dir", dir) // #nosec G204 -- fixed binary from PATH
	if err := cmd.Start(); err != nil {
		return "", err
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond); err == nil {
			_ = c.Close()
			return addr, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	return "", fmt.Errorf("redis-server did not start on %s", addr)
}
