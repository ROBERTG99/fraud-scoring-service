# Contributing

Thank you for helping. This page covers setup, the test-first workflow, the checks to run, the code
conventions, how to make the common kinds of change, and what never to do. The rules come from
[`CLAUDE.md`](CLAUDE.md) and the path-scoped rules in [`.claude/rules/`](.claude/rules/); where this page and
those files disagree, those files win.

The project is pre-release.

## Setup

You need Go, Python 3.12 or 3.13 (`pyproject.toml`: `>=3.12,<3.14`) with [uv](https://docs.astral.sh/uv/),
Node 22 with pnpm (`web/package.json` pins `pnpm@10.28.0`) and Docker. `go.mod` pins `toolchain go1.26.10`
and the Makefile sets `GOTOOLCHAIN=auto`, so any recent Go fetches the right toolchain (D-042). The versions
the project's environment records are in [`docs/DEVELOPMENT.md`](docs/DEVELOPMENT.md).

```bash
make bootstrap
```

`make bootstrap` runs `scripts/bootstrap.sh` (idempotent): it installs the Go tools (buf, the protoc plugins,
ghz, goimports, golangci-lint, gosec, govulncheck, actionlint, gitleaks, goose), syncs the locked Python
environment (`uv sync --frozen`), installs the console dependencies (`pnpm -C web install --frozen-lockfile`),
installs pinned release binaries (terraform, tflint, hadolint, trivy) into `.cache/bin`, and creates `.env`
from `.env.example`. Put `$(go env GOPATH)/bin` and `.cache/bin` on your `PATH`.

The integration suites need Postgres, Redis and Redpanda. `make test` starts them from `compose.test.yaml`
(Postgres on 8521, Redis on 8531, Redpanda on 8543) and sets the `FRAUD_TEST_*` variables itself.

All data is synthetic. Regenerate the git-ignored data and model with `make gen-data SIZE=demo` and
`make train`.

## Workflow: test first

Every change follows the loop in [`.claude/skills/tdd/SKILL.md`](.claude/skills/tdd/SKILL.md):

1. Write the failing test, named after the behaviour. Run it and check that it fails for the right reason.
2. Make the smallest change that passes it.
3. Run the area checks: `go test -race ./<pkg>/...`, `uv run pytest <path>` or `pnpm -C web test`.
4. Run `make check-fast`.
5. Commit with a Conventional Commit message.

A bug fix comes with a regression test that fails without the fix. Never leave the branch red between
commits.

When something is ambiguous, decide, and record the decision in [`docs/DECISIONS.md`](docs/DECISIONS.md)
(date, decision, alternatives, reason). An architectural choice also gets an ADR in [`docs/adr/`](docs/adr/)
([`new-adr`](.claude/skills/new-adr/SKILL.md) skill).

## Checks

| Target | What it runs | When |
|---|---|---|
| `make check-fast` | gofmt, ruff, `go vet`, `go test`, the unit, LLM and harness pytest suites | Before every commit |
| `make test` | Go with `-race -tags integration`, the Python unit, LLM, harness, integration and contract suites, Vitest, and the coverage gates | Before pushing |
| `make lint` / `make typecheck` / `make security` | Each group of checks on its own | When you touch that area |
| `make eval` | Offline eval suites gated against `evals/reports/baseline/`; writes `evals/REPORT.md` | When you change the model, rules or summarizer |
| `make verify` | `lint`, `typecheck`, `security`, `test`, `test-docker`, `e2e`, `eval`, `bench`, `tf-check` | The single gate before a release |

The coverage gates are at least 85% on `internal/{features,rules,decide,store}` and `src/fraud_summarizer`, and
at least 70% overall (`make test-go`, `make test-py`). The
[`quality-gate`](.claude/skills/quality-gate/SKILL.md) skill runs the gates in order and stops at the first
failure.

When you pipe a make target, use `set -o pipefail`, and do not chain the test run after a lint step with `&&`:
a failing formatter then silently skips the tests.

## Commits

Use [Conventional Commits](https://www.conventionalcommits.org/): `feat:`, `fix:`, `test:`, `docs:`, `chore:`,
`refactor:` (the history also uses `build:` and `style:`), with an optional scope such as `fix(store):`. Keep
one logical change per commit, and explain the why in the body.

Do not add `Co-Authored-By`, `Claude-Session` or "Generated with Claude Code" trailers to commits or pull
requests, and do not change `git config user.*` or the `GIT_AUTHOR_*` / `GIT_COMMITTER_*` variables
([`.claude/rules/git-commits.md`](.claude/rules/git-commits.md)).

## Code conventions

### Go

- Every I/O call (Redis, Postgres, HTTP, gRPC, Kafka) takes a `context.Context` with a deadline. Budgets:
  Score 45 ms, Redis 8 ms, model 20 ms, Postgres 30 ms.
- `go test -race` stays clean; every goroutine has an owner that stops it on shutdown.
- Wrap errors with `%w`; use sentinel errors such as `ErrUnavailable` for degradation decisions.
- Group imports with `goimports -local github.com/ROBERTG99/fraud-scoring-service`.
- Tests that need Postgres, Redis or Kafka carry `//go:build integration` and use `internal/testutil`.
- No `time.Sleep` in tests: poll with a deadline and inject clocks.
- Never import an LLM SDK in Go.

### Python

- Python 3.12+, run everything with `uv run`; dependencies are locked in `uv.lock`.
- ruff for lint and format; `mypy --strict` on `src/` and `training/`.
- Pydantic v2 models for every schema, with `extra="forbid"` on inputs we control.
- Async I/O in the summarizer, and a timeout on every network call (LLM 8 s).
- Tests run under pytest-socket: offline by default, loopback only.
- Provider SDKs are imported only in `src/fraud_summarizer/llm/` ([`.claude/rules/llm-code.md`](.claude/rules/llm-code.md)).

### Frontend

- Data only through the generated types (`web/src/api/schema.d.ts`) and the one TanStack Query layer in
  `web/src/api/`.
- Every screen has loading, empty, error-with-retry and success states.
- Colours come from `web/src/tokens.css` and are always paired with text; WCAG 2.2 AA, keyboard reachable.
- IDs and amounts use JetBrains Mono with tabular numerals.
- Vitest for components; Playwright and axe in `tests/e2e/`. The console's request budget is 2 s.

### Protobuf

- Never renumber, reuse or change the type of a field; remove fields with `reserved`.
- `buf lint` and `buf breaking` against `main` must pass.
- Run `make proto` after any change, and commit `gen/` and `web/src/api/schema.d.ts` with the proto. Never
  edit `gen/` by hand.

### Tests and docs

- Fixed seeds, injected clocks, polling with deadlines, unique Kafka topics and Redis prefixes per test.
- No retries, skips, xfail or weakened thresholds.
- Every number in the README and docs cites the command or report that produced it. Docs pass
  `make lint-md`; diagrams are Mermaid.

## Common changes

Each of these has a skill in [`.claude/skills/`](.claude/skills/) with the exact steps.

- **A fraud rule:** [`add-rule`](.claude/skills/add-rule/SKILL.md). Copy the active
  `rules/rules.vYYYY.MM.N.yaml` to a new version, add the rule (unique `id`, `when`, `action`,
  `reason_code`; at most 64 rules), run `go test ./internal/rules/...`, add a golden case through
  `scripts/gen_eval_sets.py`, then `make eval-decisions`.
- **A database migration:** [`add-migration`](.claude/skills/add-migration/SKILL.md). Add
  `migrations/NNNNN_<name>.sql` with goose Up and Down sections, grant each service role only what it needs
  (never UPDATE or DELETE on `audit_log`), and extend the up/down/up integration test in `internal/store`.
- **An API endpoint:** [`add-endpoint`](.claude/skills/add-endpoint/SKILL.md). Add the RPC with a
  `google.api.http` option, run `make proto`, add the method to the RBAC table in `internal/auth/rbac.go`
  with a test, implement the handler in `internal/api/` with deadlines, and add a TanStack Query hook.
- **An eval case:** [`add-eval-case`](.claude/skills/add-eval-case/SKILL.md). Extend
  `scripts/gen_eval_sets.py` (dev split first) and regenerate; test partitions are never edited by hand. If a
  test partition changes, record why in `docs/DECISIONS.md` and refresh the baseline.
- Also: [`add-llm-route`](.claude/skills/add-llm-route/SKILL.md),
  [`add-fraud-pattern`](.claude/skills/add-fraud-pattern/SKILL.md),
  [`train-and-register-model`](.claude/skills/train-and-register-model/SKILL.md) and
  [`eval-report`](.claude/skills/eval-report/SKILL.md).

## Never do

- Run `terraform apply` or `terraform destroy`, or deploy to any cloud account.
- Create, publish or change the visibility of a GitHub repository, or force-push.
- Send messages to real recipients (email, SMS, Slack, voice).
- Commit secrets, `.env` or real personal data. Use synthetic data only.
- Put an LLM, or any network call other than the model server, Redis or Postgres, on the scoring path. The
  LLM may never change, delay or block a decision.
- Skip, disable, xfail or weaken a test or threshold to get green. Fix the code, or document the gap in
  [`docs/KNOWN_LIMITATIONS.md`](docs/KNOWN_LIMITATIONS.md).
- Edit eval test partitions (`evals/datasets/**/test*.jsonl`) after they were generated, or tune on a test
  split.
- Invent metrics, eval scores, benchmark numbers or screenshots.
- Edit `gen/` or `evals/REPORT.md` by hand; both are generated.

## Reporting security issues

Do not open a public issue for a vulnerability. See [`SECURITY.md`](SECURITY.md).
