# AGENTS.md

Guidance for AI coding assistants working in this repository. `CLAUDE.md` has the full version; this file
holds the same essentials.

## Project

Real-time fraud scoring: Go gRPC scorer (features in Redis, calibrated GBDT over ONNX, YAML rules) plus an
asynchronous Python case summarizer that uses an LLM only for analyst summaries. Synthetic data only.

## Commands

- Setup: `make bootstrap`
- Fast loop: `make check-fast` (lint + unit tests, under 60 s)
- Full host tests: `make test`; in containers: `make test-docker`; browser: `make e2e`
- Gate: `make verify` (lint, typecheck, security, test, test-docker, e2e, eval, bench, tf-check)
- Stack: `make up`, `make seed`, `make demo`, `make down`

## Rules

1. Test first, small verified increments, Conventional Commits, branch never red.
2. No commit or PR attribution trailers (`Co-Authored-By`, `Claude-Session`, "Generated with Claude Code");
   never change git identity or `GIT_AUTHOR_*` / `GIT_COMMITTER_*`.
3. Never: `terraform apply|destroy`, cloud deploys, repository creation or visibility changes, force pushes,
   messages to real recipients, secrets or real personal data in commits.
4. No LLM or external network call on the scoring path; provider SDKs only in `src/fraud_summarizer/llm/`.
5. Never weaken tests or thresholds; document gaps in `docs/KNOWN_LIMITATIONS.md`.
6. Every I/O call in Go has a deadline; `-race` clean.
7. Protobuf field numbers are never renumbered.
8. Numbers in docs must cite the command or report that produced them.

## Map

`cmd/` binaries · `internal/` Go packages · `proto/` + `gen/` contracts · `schemas/`, `features/`, `rules/`
· `training/`, `serving/` · `src/fraud_summarizer/` · `prompts/`, `evals/` · `migrations/`, `deploy/`,
`infra/terraform/` · `web/` · `tests/`.

Decisions: `docs/DECISIONS.md`. ADRs: `docs/adr/`. Changes: `CHANGELOG.md`.
