# ADR 0002: The scorer is a Go gRPC service, not a Python one

## Status

Accepted.

## Date

2026-10-18

## Context

`Score` sits in the card authorization path. The spec gives it a 45 ms deadline with nested budgets for
Redis (8 ms), the model (20 ms) and Postgres (30 ms), a p95 target under 50 ms on a laptop at 2,000 RPS
(NFR-1) and under 80 ms in CI at 300 RPS (NFR-2), and a 1,000 RPS durability target (NFR-7). Every I/O
call must carry its own deadline, and the handler must keep answering, rules-only, when the model or Redis
is slow. The machine learning side (training, calibration, ONNX export, MLflow, the LLM summarizer) lives in
the Python ecosystem. The spec's tech-stack table (§5) picks Go for the scorer and the feature-updater for
low tail latency and lists Python as the rejected alternative; this record states that choice and what it
cost.

## Decision

The latency-critical services are Go: `cmd/scorer` (gRPC `fraud.v1.Scorer` and `fraud.v1.CaseService` with
grpc-gateway for HTTP, both from `proto/fraud/v1/` through buf), `cmd/feature-updater`, and the operational
tools (`cmd/replay`, `cmd/seed`, `cmd/retention`, `cmd/webserve`, `cmd/dev-issuer`). They use grpc-go,
go-redis, pgx and franz-go. Python is used where its libraries are the point: `training/`, the BentoML model
server (`serving/bentoml/`) and the case summarizer (`src/fraud_summarizer/`).

- The budgets are constants in `internal/api/scorer.go:33-38`; each I/O step derives its context from the
  45 ms Score context (`internal/api/scorer.go:124`, `:152`, `:167`, `:213`, `:250`).
- The model is reached over HTTP through `internal/model.Client` (ADR 0004, ADR 0005), so the Go process
  never loads a Python runtime.
- Go is pinned to 1.26.10 with `GOTOOLCHAIN=auto` (docs/DECISIONS.md D-042, which supersedes D-018's 1.24
  pin). The binary is static and runs on `distroless/static-debian12:nonroot` (`Dockerfile.go`, D-055).

## Alternatives considered

- **A Python scorer (FastAPI or grpcio) calling the same model server.** One language for everything and no
  duplicated feature code. Rejected by the spec for tail latency; this repository has not built or measured
  one, so there is no number here comparing the two.
- **A Python scorer with onnxruntime in process.** Removes the network hop to the model server, but puts
  per-request Python overhead and the model's CPU work in the same process as Redis, Postgres and Kafka I/O.
  Not built or measured.

## Consequences

Positive:

- Deadlines are explicit per call and enforced by `context.WithTimeout`; the rules-only fallback is a branch
  in one function (`internal/api/scorer.go:163-180`). `TestModelTimeoutRulesOnlyWithin35ms` shows a 50 ms
  model stub answered under 35 ms.
- Measured on the host stack: `make bench` (ci profile, 300 rps, 60 s) gives p50 11.3 ms, p95 19.6 ms, p99
  30.5 ms (`evals/REPORT.md`, latency). With tracing off, `BenchmarkScore` makes 79 allocations per call
  (`go test -run xxx -bench BenchmarkScore -benchmem -count 3 ./internal/api`, D-056).
- The feature-updater applies 57,359 events/s against a budget of 10,000 (`BenchmarkUpdater`, `PROGRESS.md`
  T-12).
- One protobuf source gives the gRPC server, the HTTP gateway, the OpenAPI document and the console's
  TypeScript types (`scripts/proto.sh`, D-004, D-020).
- `go test -race` runs over the whole Go code base (CLAUDE.md conventions).

Negative:

- Two languages implement the same contracts. The 13 online features are computed in Go and Lua
  (`internal/features/store.go`, `internal/features/score_tx.lua`) and offline in Python
  (`training/features.py`); `make feature-parity` guards them (3,502 transactions × 13 features, 0
  mismatches, `PROGRESS.md` T-13). Event schemas are validated in both languages against shared fixtures
  (`PROGRESS.md` T-07).
- The model is a network hop away, behind a Python server that is the throughput limit on a shared 4-CPU
  host: the laptop profile achieved 586 of 2,000 rps with p95 603.8 ms (`make bench BENCH_PROFILE=laptop`,
  `PROGRESS.md` T-39, `docs/KNOWN_LIMITATIONS.md`). The Go scorer's speed does not remove that limit.
- Toolchain churn is real: the move from Go 1.24 to 1.26 was needed to clear 48 HIGH or CRITICAL findings
  that trivy reported in the 1.24.13 binary (D-042).
