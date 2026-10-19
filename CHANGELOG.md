# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project will adhere
to [Semantic Versioning](https://semver.org/spec/v2.0.0.html) once it has a first release. No version has
been tagged yet. Entries are grouped by milestone of the build specification
([`docs/specs/000-build-spec.md`](docs/specs/000-build-spec.md) §8); task IDs, evidence and the numbers
quoted below come from [`PROGRESS.md`](PROGRESS.md), decisions from [`docs/DECISIONS.md`](docs/DECISIONS.md).

## Unreleased

### Changed

- evalgate is now the released `v0.1.0` (`git+https://github.com/ROBERTG99/evalgate@v0.1.0`, locked at
  0681335 in `uv.lock`); the vendored stand-in in `third_party/evalgate/` is removed (D-072).

### Pending: M7 to M9

Not started (`PROGRESS.md`, Tasks):

- **M7:** images, compose profiles and the test compose file for `make test-docker` (T-43), the SageMaker
  Terraform module (T-44), CI workflows (T-45).
- **M9:** durability and chaos tests (T-48), final reviews and fixes (T-49), final verification (T-50).

### M8: demo and documentation (T-46, T-47)

#### Added

- `make demo` (`scripts/demo.sh`, `cmd/replay/scenario.go`): the spec §3.4 journeys as 13 checkpoints
  against the host stack, offline. A USD 940 purchase from a new device two minutes after an email change is
  REVIEWed and summarized. The run also covers an idempotent replay, a card-testing burst, an invalid event
  in the DLQ and the shadow comparison. The model server is stopped for rules-only decisions and then
  restarted. `tests/e2e/demo.spec.ts` asserts the checkpoints and labels the case in the console (D-071).
- The documentation set of spec §18, written ahead of M7 at the maintainer's request (D-070).

### M6: telemetry and security (T-41, T-42)

#### Added

- OpenTelemetry tracing of `Score` end to end (`fraud.score` with feature, model, rules and Postgres child
  spans), exported only when `OTEL_EXPORTER_OTLP_ENDPOINT` is set; the W3C `traceparent` is stored with the
  outbox row and sent as a Kafka header, so a case summary joins its decision's trace (D-038).
- Summarizer spans per pipeline step with `gen_ai.*` attributes, and the `fraud_llm_cost_usd_total` counter.
- The `observability` compose profile (OTel Collector, Prometheus, Grafana, Jaeger), a generated Grafana
  dashboard, SLO alerts with unit tests (`ScoreLatencyHigh`, `DegradedDecisions`, `OutboxUnsentAge`) and
  `docs/runbook.md`.
- `make security` (`scripts/security.sh`): gitleaks, gosec, govulncheck, bandit, pip-audit, pnpm audit,
  hadolint, checkov, trivy and a licence check; it fails closed when a scanner cannot run.
- `docs/security.md`: trust boundaries, STRIDE per component, OWASP Top 10 for LLM applications, scan results,
  accepted and open findings.
- `cmd/retention`: age-based deletion in batches; the audit log is never pruned (D-040).

#### Security

Findings of the M6 security audit and nine review rounds, each fixed with a failing-first test (D-041 to
D-069):

- `event_ts` bounded to the last 24 hours and 5 minutes ahead (D-041); rule expressions limited to bounded
  work; MCC allowlist on Score, Kafka and OpenAPI (D-051).
- One least-privilege Postgres role per service (D-043); services refuse development database passwords
  outside dev mode (D-059, D-063).
- Idempotent replays checked against a request hash and Redis values protected by an HMAC (D-044, D-057,
  D-062).
- The dev-issuer and plain-HTTP JWKS only with `FRAUD_ENV=dev` (D-045); JWKS refresh single-flight with a
  stale cap (D-053).
- gRPC metadata, stream, handshake and ping limits, and at most 4 live streams per user (D-046).
- The model server verifies the ONNX SHA-256 before loading (D-047, D-059, D-063).
- Caps on paid summary attempts and consumer retries, and a daily LLM budget (D-048, D-059, D-062).
- A keyed audit chain (HMAC-SHA256) with signed anchors outside the database (D-050, D-057, D-061).
- Metrics and detailed readiness on admin listeners only (D-052, D-054); compose ports on loopback,
  Grafana without default credentials (D-049); read-only, capability-free containers with base images pinned
  by digest (D-055).
- The PII redactor rewritten around digit-group windows and complete decoding of percent escapes and HTML
  entities, failing closed when it cannot converge (D-058, D-061, D-063 to D-069).
- Faithfulness requires a statement to cite its fact's value and drops case-clearing language (D-059,
  D-061, D-062, D-064 to D-067).

#### Changed

- Go 1.24 to 1.26.9 and the vulnerable Go modules upgraded; trivy reported 48 HIGH or CRITICAL findings in
  the scorer binary before and 0 after (D-042, maintainer's choice, deviating from the spec's Go 1.24).
- Grafana 11.5.2 to 12.4.12 (D-049); Python images on `python:3.12.15-slim` (D-055).

#### Open

- govulncheck has not run: the build environment cannot reach `vuln.go.dev`, so `make security` fails closed
  (`PROGRESS.md`, T-42). The other open findings are listed in `docs/security.md`.

### M5: evaluation (T-36 to T-40)

#### Added

- A vendored `evalgate` in `third_party/evalgate/` (`run` and `check`, on upstream's suite, report and check
  contracts), because the upstream `v0.1.0` tag does not exist (D-002, D-030).
- Eval datasets generated by `scripts/gen_eval_sets.py` with seed 7: 150 decision cases, 120 summary cases
  and 80 judge-calibration items, with a manifest of hashes (D-031).
- `cmd/evaldecide`, which runs the real scorer for the decisions suite (D-032).
- The `model` suite on the temporal test split of the 2M-event data, `decisions` and `summaries` suites with
  graders, the `Score` latency bench (`cmd/benchreport`, `scripts/bench.sh`), and a usefulness judge.
- `scripts/build_report.py`, which writes `evals/REPORT.md`; baselines in `evals/reports/baseline/` (D-035);
  `make eval` gates every suite against them.
- `make eval-live` and `make record-fixtures` with a USD budget; not run here, since no provider key is set.

#### Fixed

- The model suite no longer fits a threshold on the test split; it gates recall at the valid-chosen and the
  shipped thresholds and a band calibration gap (D-037).
- Gates fail closed on a missing baseline or suite, and judge calibration output stays out of the report glob
  (`PROGRESS.md`, M5 row).
- FakeLLM heuristics and the redactor tuned on the dev split only (`PROGRESS.md`, T-38).

### M4: analyst console (T-32 to T-35)

#### Added

- The React console in `web/` (Vite, React 19, TanStack Query 5, Tailwind 4) on generated API types, served
  by `cmd/webserve` with a CSP and API proxies.
- Screens: sign-in, review queue, case detail with the "AI summary · does not affect decisions" panel and a
  label dialog that previews the row to be written, account timeline, model and shadow, live metrics, rules
  with "Test rule" for risk engineers.
- Playwright end-to-end tests with axe (WCAG 2.0 to 2.2 A and AA) and console screenshots in `docs/images/`
  (D-028).

#### Fixed

- The summarizer could not decode snappy-compressed Kafka batches and died silently; provider errors no
  longer stall the consumer (`PROGRESS.md`, T-35).
- The shadow screen showed completed comparisons as drops (`PROGRESS.md`, T-35).
- The M4 accessibility and UX blockers: queue cursor, label-dialog focus, timeline amounts shown 100 times
  too large, input border contrast, skip link, short IDs (`PROGRESS.md`, M4 row).

### M3: scoring service (T-23 to T-31)

#### Added

- The `Score` handler with validation, service-token auth and idempotency through Redis with a Postgres
  fallback (D-017).
- Degraded paths: rules-only scoring on model timeout, and a REVIEW bias by amount when Redis is down (D-012,
  D-013).
- A group-commit Postgres writer with a disk spool, and an at-least-once outbox relay to Kafka.
- Bounded asynchronous shadow scoring with comparisons.
- `CaseService` with EdDSA JWTs, RBAC and `cmd/dev-issuer`; the scorer serves gRPC and an HTTP gateway with a
  strict OpenAPI contract tested by Schemathesis (D-020).
- The summarizer's Kafka consumer, writing summaries for REVIEW decisions, with an idempotent regenerate API
  (D-022).
- A SageMaker training launcher built with `boto3` only, with a dry run and a moto test (D-003).
- `cmd/seed` and `cmd/replay`, with batched model calls per alias (D-024).

#### Changed

- Synthetic generator v2 (legitimate look-alikes, low-and-slow fraud) and feature spec v2 (`new_device` means
  first seen less than 24 hours ago); data, model and every dependent number regenerated (D-026, ADR 0001).

#### Fixed

- Replay leaked later device sightings into features; it now applies each account's events in order before
  scoring that account, and the open-case numbers from the leaking replay were withdrawn (D-024, D-025).
- A hung Postgres overloaded the writer; a breaker now spools to disk and closes under continuous traffic
  (D-027).

### M2: model, rules and LLM pipeline (T-14 to T-22)

#### Added

- XGBoost training with bounded isotonic calibration and a verified ONNX export (D-008); an MLflow registry
  with aliases on a SQLite backend (D-009).
- A BentoML model server on onnxruntime, a Go model client with deadline-bound errors, and a Triton model
  repository with a KServe v2 client (Triton not run: `nvcr.io` is blocked, `docs/KNOWN_LIMITATIONS.md`).
- A versioned YAML rules engine (expr-lang, schema validation, hot reload) and the rules-and-model
  combination table.
- An LLM gateway with Anthropic and OpenAI adapters, FakeLLM and cassettes (hand-authored in the documented
  shapes, D-011).
- The case-summary pipeline with a code-level faithfulness check and PII redaction.

#### Fixed

- The M2 review blockers: the provider schema dropped the `pattern` property, the faithfulness check missed
  numbers with units, and invalid model output was retried instead of moving to the fallback (D-014).

### M1: contracts, data and features (T-06 to T-13)

#### Added

- Protobuf contracts for `Scorer` and `CaseService` with generated Go, gateway and OpenAPI code (D-004,
  D-007).
- Event JSON Schemas with Go and Python validators that agree on shared fixtures.
- A deterministic synthetic event generator with four fraud patterns (`cmd/gen`, `cmd/gen/PATTERNS.md`).
- Goose migrations with an append-only, hash-chained audit log.
- Feature spec v1 with point-in-time offline features, a Redis Lua feature store with an atomic
  read-as-of-then-add, the feature-updater with a DLQ, and a train/serve parity check
  (`make feature-parity`).

#### Fixed

- The M1 review blockers: consumer shutdown could hang, the updater's Redis client timed out pipelines, and
  credential features were not exact (`PROGRESS.md`, M1 row).

### M0: bootstrap and harness (T-01 to T-05)

#### Added

- Commit attribution configuration, `PROGRESS.md`, `docs/DECISIONS.md` and `docs/DEVELOPMENT.md`.
- The Makefile, the Go module and the locked Python toolchain (`make bootstrap`, `make check-fast`).
- The harness: `CLAUDE.md`, `AGENTS.md`, path-scoped rules, subagents and skills in `.claude/`, and hooks
  (guard, format, session start, stop check) tested by `tests/harness/test_hooks.py`.
- A walking skeleton in Docker: a rules-only scorer and a summarizer on the fake LLM.

#### Fixed

- The M0 review blockers: `go vet ./...` parsed `Dockerfile.go`, and the `.env.*` deny rule also blocked
  `.env.example` (`PROGRESS.md`, M0 row).
