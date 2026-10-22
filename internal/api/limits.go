package api

import "time"

// Request limits. Every surface reads them from here: the gRPC server and gateway (body and
// header size), ValidateTransaction (amount, merchant name) and the scorer's per-user rate limiter.
const (
	// MaxBodyBytes caps a gRPC message and an HTTP request body: 64 KB. A larger request is rejected with
	// RESOURCE_EXHAUSTED (HTTP 413) before it is parsed.
	MaxBodyBytes = 64 << 10
	// MaxHeaderBytes caps HTTP request headers.
	MaxHeaderBytes = 16 << 10
	// MaxAmountMinor is the largest accepted amount in minor units.
	MaxAmountMinor = 10_000_000
	// MaxMerchantNameRunes bounds the only free-text field a transaction carries (it reaches the LLM prompt
	// as fenced data, after redaction).
	MaxMerchantNameRunes = 120
	// MaxEventSkew is how far ahead of the server clock event_ts may be (clock skew). event_ts drives the
	// feature windows: a far-future value would trim an account's history to nothing (security audit).
	MaxEventSkew = 5 * time.Minute
	// DefaultMaxEventAge is the oldest event_ts Score accepts: a backdated value would move a device's
	// first-seen time earlier and hide history. Replays of historical data opt out (ScorerDeps.MaxEventAge).
	DefaultMaxEventAge = 24 * time.Hour
	// DefaultUserRPS is the per-user rate for console APIs (token bucket, burst equal to the rate); the
	// service token used by Score is exempt. Override with FRAUD_RATE_LIMIT_RPS.
	DefaultUserRPS = 20
	// MaxStreamsPerUser bounds the server streams (StreamLiveMetrics) one console user holds open at once;
	// The rate limit only applies when a stream opens (security audit).
	MaxStreamsPerUser = 4
	// MaxConcurrentStreams bounds the streams on one HTTP/2 connection, including the gateway's loopback
	// connection, which carries every HTTP request.
	MaxConcurrentStreams = 1024
)
