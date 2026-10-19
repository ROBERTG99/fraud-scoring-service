---
paths:
  - "**/*.go"
  - "go.mod"
---
# Go rules

- Every I/O call (Redis, Postgres, HTTP, gRPC, Kafka) takes a `context.Context` with a deadline. Budgets:
  Score 45 ms, Redis 8 ms, model 20 ms, Postgres 30 ms.
- `go test -race` must be clean. No goroutine without an owner that stops it on shutdown.
- Return errors with `%w` wrapping; sentinel errors (`ErrUnavailable`) for degradation decisions.
- Imports: `goimports -local github.com/ROBERTG99/fraud-scoring-service`.
- Tests that need Postgres, Redis or Kafka carry `//go:build integration` and use `internal/testutil`.
- No `time.Sleep` in tests; poll with a deadline. Inject clocks.
- Never import an LLM SDK in Go.
