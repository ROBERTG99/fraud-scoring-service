# CLAUDE.md — fraud-scoring-service

Real-time card-fraud scoring. A Go gRPC scorer decides each transaction (APPROVE / REVIEW / DECLINE) from
Redis point-in-time features, a calibrated GBDT served over ONNX (BentoML, optional Triton) and versioned
YAML rules. A Python worker writes asynchronous LLM case summaries for REVIEW decisions; the LLM is never on
the scoring path. All data is synthetic (`cmd/gen`, seed 7).

## Where things are

| Path | What |
|---|---|
| `cmd/` | scorer, feature-updater, gen, replay, seed, dev-issuer, webserve, retention, benchreport, loadgen |
| `internal/api` | gRPC/HTTP handlers, interceptors (auth, limits, rate limit) |
| `internal/features` | Redis Lua feature store (`score_tx.lua`), feature spec, fallback defaults |
| `internal/model` | model clients (BentoML, Triton KServe v2), shadow runner |
| `internal/rules`, `internal/decide` | rules engine (expr-lang), combination table |
| `internal/store` | Postgres group-commit writer, spool, outbox relay, cases, audit log |
| `internal/stream`, `internal/auth`, `internal/telemetry` | Kafka + JSON Schema validation; JWT + RBAC; OTel + Prometheus |
| `proto/`, `gen/` | protobuf sources; generated Go, gateway, OpenAPI (never edit `gen/` by hand) |
| `schemas/`, `features/`, `rules/` | JSON Schemas, feature spec `v1`, versioned rule sets |
| `training/`, `serving/` | dataset, offline features, train, ONNX export, evaluate, registry, SageMaker; BentoML and Triton |
| `src/fraud_summarizer/` | summarizer app, consumer, pipeline, `llm/` gateway (only place that imports provider SDKs) |
| `prompts/`, `evals/` | versioned prompts; eval datasets, suites, graders, reports, `REPORT.md` |
| `migrations/`, `deploy/`, `infra/terraform/` | goose SQL; otel/prometheus/grafana; SageMaker training module |
| `web/` | React + Vite analyst console |
| `tests/{unit,integration,contract,e2e,harness,llm}` | test suites by layer |

## Make targets

`make bootstrap` · `make up` / `make down` · `make seed` · `make demo` · `make check-fast` (lint + unit, < 60 s) ·
`make test` (host unit, integration, contract) · `make test-docker` · `make e2e` · `make eval` · `make bench` ·
`make lint` / `make typecheck` / `make security` · `make verify` (the single gate) · `make verify-clean` ·
`make logs` · `make eval-live` · `make record-fixtures` · `make triton-up` · `make retention` · `make dlq-peek` ·
`make proto` · `make gen-data SIZE=demo|eval` · `make train` · `make register-model ALIAS=production|shadow` ·
`make feature-parity` · `make tf-check` · `make sagemaker-dry-run` · `make audit-verify` · `make clean`.

## Conventions

- **Test first.** Write the failing test, implement, run the area checks, record evidence in `PROGRESS.md`,
  commit with a Conventional Commit (`feat:`, `fix:`, `test:`, `docs:`, `chore:`, `refactor:`). Never leave
  the branch red between tasks.
- Go: every I/O call has a deadline (`context.WithTimeout`); `go test -race` must stay clean; imports grouped
  with `goimports -local github.com/ROBERTG99/fraud-scoring-service`.
- Python 3.12+, `uv run`, ruff, `mypy --strict` on `src/` and `training/`, Pydantic v2 for every schema.
- Frontend: generated API types only (`web/src/api/schema.d.ts`), one TanStack Query layer, tokens in
  `web/src/tokens.css`, WCAG 2.2 AA.
- Protobuf: never renumber or reuse field numbers; `buf breaking` against `main`.
- Timeouts: Score 45 ms, Redis 8 ms, model 20 ms, Postgres 30 ms, LLM 8 s, console 2 s.
- Every number in README/docs comes from a command or report that is cited next to it.
- Ambiguity: decide, record in `docs/DECISIONS.md`; architectural choices also get an ADR in `docs/adr/`.

## Commit attribution (spec §1.1)

Commits and pull requests go out under the maintainer's name only. Never add `Co-Authored-By`,
`Claude-Session` or "Generated with Claude Code" trailers. Never modify `git config user.*` or the
`GIT_AUTHOR_*` / `GIT_COMMITTER_*` variables. See `.claude/rules/git-commits.md`.

## Never do

- `terraform apply` / `terraform destroy`; deploy to any cloud account; create, publish or change the
  visibility of a GitHub repository; force-push.
- Send messages to real recipients (email, SMS, Slack, voice).
- Commit secrets, `.env`, or real personal data. Synthetic data only.
- Put an LLM (or any network call that is not the model server, Redis or Postgres) on the scoring path.
  The LLM may never change, delay or block a decision.
- Skip, disable, xfail or weaken a test or threshold to get green. Fix the code or document the gap in
  `docs/KNOWN_LIMITATIONS.md`.
- Edit files in eval test partitions (`evals/datasets/**/test*.jsonl`) after they were generated.
- Invent metrics, eval scores, benchmark numbers or screenshots.

## Harness

- Subagents in `.claude/agents/`: `reviewer` (verdict "ship" / "fix first"), `security-auditor`,
  `test-designer`, `eval-analyst`, `ux-reviewer`, `docs-editor`, `latency-reviewer`, `ml-reviewer`.
- Skills in `.claude/skills/`: `tdd`, `quality-gate`, `eval-report`, `new-adr`, `add-migration`,
  `add-endpoint`, `add-llm-route`, `add-eval-case`, `demo-capture`, `release`, `add-rule`,
  `train-and-register-model`, `add-fraud-pattern`.
- Hooks in `.claude/hooks/` (tested by `tests/harness/test_hooks.py`): SessionStart (cloud only),
  PostToolUse format, PreToolUse guard, Stop check.
- Path-scoped rules in `.claude/rules/`.

## Key documents

- Specification: `docs/specs/000-build-spec.md` · Progress: `PROGRESS.md` · Decisions: `docs/DECISIONS.md`
- ADRs: `docs/adr/` · Architecture: `docs/architecture.md` · Design FAQ: `docs/design-faq.md`
- Evaluation: `docs/evaluation.md`, `evals/REPORT.md` · Security: `docs/security.md` · Runbook: `docs/runbook.md`
- Known limitations: `docs/KNOWN_LIMITATIONS.md` · Verification: `docs/verification.md`

After a context reset: re-read the spec, `PROGRESS.md`, `docs/DECISIONS.md` and `git log --oneline -30`,
then resume from the first unchecked task.
