package api

// Request limits (spec §4.4, §15). Every surface reads them from here: the gRPC server and gateway (body and
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
	// DefaultUserRPS is the per-user rate for console APIs (token bucket, burst equal to the rate); the
	// service token used by Score is exempt. Override with FRAUD_RATE_LIMIT_RPS.
	DefaultUserRPS = 20
)
