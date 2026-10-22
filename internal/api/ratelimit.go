package api

import (
	"context"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RateLimiter is a per-console-user token bucket. The service principal
// (Score) is not limited here; Score is protected by the writer queue and ResourceExhausted instead.
type RateLimiter struct {
	rate  float64
	burst float64
	now   func() time.Time
	mu    sync.Mutex
	users map[string]*bucket
	open  map[string]int // live streams per user, bounded by MaxStreamsPerUser
}

type bucket struct {
	tokens float64
	last   time.Time
}

// maxTrackedUsers bounds memory; the map is reset when it is exceeded (buckets refill anyway).
const maxTrackedUsers = 10_000

// NewRateLimiter allows rps requests per second per user with a burst of rps.
func NewRateLimiter(rps float64, now func() time.Time) *RateLimiter {
	if now == nil {
		now = time.Now
	}
	return &RateLimiter{rate: rps, burst: rps, now: now, users: map[string]*bucket{}, open: map[string]int{}}
}

// Allow takes one token for subject.
func (r *RateLimiter) Allow(subject string) bool {
	if r == nil || r.rate <= 0 {
		return true
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.users[subject]
	if !ok {
		if len(r.users) >= maxTrackedUsers {
			r.users = map[string]*bucket{}
		}
		b = &bucket{tokens: r.burst, last: now}
		r.users[subject] = b
	}
	b.tokens = min(r.burst, b.tokens+now.Sub(b.last).Seconds()*r.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (r *RateLimiter) check(ctx context.Context) error {
	p, ok := PrincipalFrom(ctx)
	if !ok || p.Role == "service" {
		return nil
	}
	if !r.Allow(p.Subject) {
		return status.Error(codes.ResourceExhausted, "rate limit exceeded")
	}
	return nil
}

// Unary limits unary calls; it must run after the auth interceptor.
func (r *RateLimiter) Unary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := r.check(ctx); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// acquireStream takes one of subject's MaxStreamsPerUser stream slots; release returns it.
func (r *RateLimiter) acquireStream(subject string) (release func(), ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.open[subject] >= MaxStreamsPerUser {
		return nil, false
	}
	r.open[subject]++
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.open[subject]--; r.open[subject] <= 0 {
			delete(r.open, subject)
		}
	}, true
}

// Stream limits stream opens and the streams a console user holds open at once; it must run after the auth
// interceptor. The stream cap applies even when the request rate limit is off.
func (r *RateLimiter) Stream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := r.check(ss.Context()); err != nil {
			return err
		}
		if p, ok := PrincipalFrom(ss.Context()); ok && p.Role != "service" && r != nil {
			release, ok := r.acquireStream(p.Subject)
			if !ok {
				return status.Errorf(codes.ResourceExhausted, "at most %d open streams per user", MaxStreamsPerUser)
			}
			defer release()
		}
		return handler(srv, ss)
	}
}
