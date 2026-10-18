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

type ctxStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s ctxStream) Context() context.Context { return s.ctx }

// A rate limit on stream opens does not bound how many stay open: each live stream costs the scorer a
// snapshot every 2 s, so a user may hold only MaxStreamsPerUser at once (audit M6 M5).
func TestConcurrentStreamsPerUserAreCapped(t *testing.T) {
	rl := NewRateLimiter(1000, nil)
	release := make(chan struct{})
	opened := make(chan struct{}, 2*MaxStreamsPerUser)
	h := func(any, grpc.ServerStream) error {
		opened <- struct{}{}
		<-release
		return nil
	}
	open := func(subject string) chan error {
		done := make(chan error, 1)
		ss := ctxStream{ctx: WithPrincipal(context.Background(), Principal{Subject: subject, Role: "analyst"})}
		go func() { done <- rl.Stream()(nil, ss, &grpc.StreamServerInfo{}, h) }()
		return done
	}
	var held []chan error
	for range MaxStreamsPerUser {
		held = append(held, open("analyst-1"))
		<-opened
	}
	if err := <-open("analyst-1"); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("stream %d for one user: %v, want ResourceExhausted", MaxStreamsPerUser+1, err)
	}
	other := open("risk-1") // another user has their own allowance
	<-opened
	close(release) // every held stream ends
	for _, d := range append(held, other) {
		if err := <-d; err != nil {
			t.Fatal(err)
		}
	}
	again := make(chan struct{})
	ss := ctxStream{ctx: WithPrincipal(context.Background(), Principal{Subject: "analyst-1", Role: "analyst"})}
	if err := rl.Stream()(nil, ss, &grpc.StreamServerInfo{}, func(any, grpc.ServerStream) error { close(again); return nil }); err != nil {
		t.Fatalf("a slot freed by a closed stream must be reusable: %v", err)
	}
	<-again
}
