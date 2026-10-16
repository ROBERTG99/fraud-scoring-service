package api

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRateLimiterPerUser(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	rl := NewRateLimiter(20, func() time.Time { return now })
	for i := range 20 {
		if !rl.Allow("analyst-1") {
			t.Fatalf("request %d denied within burst", i)
		}
	}
	if rl.Allow("analyst-1") {
		t.Fatal("21st request in the same instant must be denied")
	}
	if !rl.Allow("risk-1") {
		t.Fatal("another user has their own bucket")
	}
	now = now.Add(100 * time.Millisecond) // refills 2 tokens
	got := []bool{rl.Allow("analyst-1"), rl.Allow("analyst-1"), rl.Allow("analyst-1")}
	if !got[0] || !got[1] || got[2] {
		t.Fatalf("refill must follow the configured rate: %v", got)
	}
}

func TestRateLimiterSkipsServiceAndMapsToResourceExhausted(t *testing.T) {
	rl := NewRateLimiter(1, func() time.Time { return time.Unix(0, 0) })
	h := func(context.Context, any) (any, error) { return "ok", nil }
	info := &grpc.UnaryServerInfo{FullMethod: "/fraud.v1.CaseService/ListCases"}
	user := WithPrincipal(context.Background(), Principal{Subject: "analyst-1", Role: "analyst"})
	if _, err := rl.Unary()(user, nil, info, h); err != nil {
		t.Fatal(err)
	}
	if _, err := rl.Unary()(user, nil, info, h); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("over limit: %v", err)
	}
	svc := WithPrincipal(context.Background(), Principal{Subject: "service", Role: "service"})
	for range 5 {
		if _, err := rl.Unary()(svc, nil, info, h); err != nil {
			t.Fatalf("service must not be limited: %v", err)
		}
	}
}
