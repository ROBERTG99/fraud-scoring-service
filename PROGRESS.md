# PROGRESS

Build progress for `fraud-scoring-service`, driven by `docs/specs/000-build-spec.md` (originally `BUILD.md`).

## Needs the maintainer

- **evalgate tag missing.** `git ls-remote https://github.com/ROBERTG99/evalgate` on 2026-10-16 shows only `refs/heads/main` (no `v0.1.0` tag, no code yet). Per §11 the consumer dependency rule applies: a minimal report.v1-compatible implementation lives in `third_party/evalgate/`. When `v0.1.0` is published, switch `pyproject.toml` to `evalgate @ git+https://github.com/ROBERTG99/evalgate@v0.1.0`, point `.github/workflows/evals.yml` at `ROBERTG99/evalgate/action@v0.1.0`, and delete `third_party/evalgate/`.

## Attribution evidence (§1.1)

Environment: Claude Code on the web (`CLAUDE_CODE_REMOTE=true`). `GIT_AUTHOR_NAME`, `GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_NAME`, `GIT_COMMITTER_EMAIL` are all set.

`git log -1 --format='%an <%ae> | %cn <%ce>%n%B'` after `chore: configure commit attribution`:

```text
ROBERTG99 <54124633+ROBERTG99@users.noreply.github.com> | ROBERTG99 <54124633+ROBERTG99@users.noreply.github.com>
chore: configure commit attribution
```

## Tasks

| ID | Title | Milestone | Status | Evidence |
|---|---|---|---|---|
| T-01 | Bootstrap and attribution evidence | M0 | done | `uv run pytest tests/harness/test_repo_files.py` → 2 passed; §1.1 evidence above |
| T-02 | Makefile and toolchains | M0 | done | `make bootstrap` → done in 35 s; `make check-fast` → exit 0 (go test ok, 41 pytest passed); `test_all_block_h_targets_declared` passes |
| T-03 | Harness docs, rules, subagents, skills | M0 | done | harness files exist; `uv run pytest tests/harness/test_harness_docs.py` passes; reviewer ran on M0 (see Milestone reviews) |
| T-04 | Hooks with tests | M0 | done | `uv run pytest tests/harness/test_hooks.py` → 35 passed (force push, test-partition edit, attribution, identity, terraform, rm -rf, .env); session_start.sh run with CLAUDE_CODE_REMOTE=true → rc 0, idempotent |
| T-05 | Walking skeleton in Docker | M0 | done | `go test ./cmd/scorer -run TestScoreReturnsRulesOnlyDecision` → ok; `docker compose up -d --build --wait` → scorer + case-summarizer healthy (41 s); `curl localhost:8511/healthz` → 200; `make check-fast` → exit 0 after M0 review fixes |
| T-06 | Protobuf and codegen | M1 | done | `./scripts/proto.sh` twice → identical `gen/` (diff empty); `buf lint` clean; `go test ./tests/contract -run TestDecisionFieldsStable` → ok |
| T-07 | Event schemas and validators | M1 | done | `go test ./internal/stream` (TestInvalidEventRejected, TestSharedFixtures 40 fixtures) → ok; `uv run pytest tests/unit/test_events.py::test_shared_fixtures_agree` → pass (20 valid, 20 invalid) |
| T-08 | Synthetic generator | M1 | done | `go test ./cmd/gen` (TestDeterministicSHA, TestFraudRateWithinTolerance, schema/order/split tests) → ok; `make gen-data SIZE=demo` → 200,000 events in 3.1 s, fraud rate 0.0080, sha256 ccd80914a827 |
| T-09 | Migrations | M1 | done | `go test -tags integration ./internal/store` (TestUpDownUp, TestAuditLogRejectsUpdate, TestDecisionConstraints) → ok on Postgres 16 (testcontainers and compose.test.yaml) |
| T-10 | Feature spec and offline features | M1 | done | `uv run pytest tests/unit/test_features.py` (Hypothesis test_point_in_time_excludes_future_events + vectorized vs naive, 150 examples each) → 4 passed; demo features in 1.2 s |
| T-11 | Redis Lua feature store | M1 | done | `go test -race -tags integration ./internal/features/` (TestReadAsOfThenAddIsAtomic: 1 round trip per ScoreTx, 64 concurrent; TestOutOfOrderWithinFiveMinutes rapid 100 cases) → ok |
| T-12 | Feature-updater and DLQ | M1 | done | `go test -race ./cmd/feature-updater -run TestUpdaterRoutesInvalidToDLQ` (kfake) → ok; `go test -tags integration -bench BenchmarkUpdater -benchtime 3x ./cmd/feature-updater` → 57,848 events/s (budget ≥ 10,000) |
| T-13 | Train/serve feature parity | M1 | done | `make feature-parity` → 3430 transactions x 13 features, 0 mismatches (Go TestOnlineOfflineParity vs training/features.py) |
| T-14 | Training, calibration, ONNX export | M2 | todo | |
| T-15 | MLflow registry | M2 | todo | |
| T-16 | BentoML model server | M2 | todo | |
| T-17 | Go model client | M2 | todo | |
| T-18 | Triton profile and KServe v2 client | M2 | todo | |
| T-19 | Rules engine | M2 | todo | |
| T-20 | Combination table | M2 | todo | |
| T-21 | LLM gateway and FakeLLM | M2 | todo | |
| T-22 | Summary pipeline and faithfulness | M2 | todo | |
| T-23 | Score handler with idempotency | M3 | todo | |
| T-24 | Degradation paths | M3 | todo | |
| T-25 | Group commit and spool | M3 | todo | |
| T-26 | Outbox relay | M3 | todo | |
| T-27 | Shadow mode | M3 | todo | |
| T-28 | CaseService, RBAC, dev-issuer | M3 | todo | |
| T-29 | Summarizer consumer | M3 | todo | |
| T-30 | SageMaker launcher | M3 | todo | |
| T-31 | Seed job and replay | M3 | todo | |
| T-32 | Console scaffold and data layer | M4 | todo | |
| T-33 | Queue and case detail | M4 | todo | |
| T-34 | Timeline, shadow, live metrics, rules | M4 | todo | |
| T-35 | E2E, axe, screenshots | M4 | todo | |
| T-36 | Eval datasets | M5 | todo | |
| T-37 | Model eval report | M5 | todo | |
| T-38 | Decision and summary suites | M5 | todo | |
| T-39 | Latency bench | M5 | todo | |
| T-40 | REPORT.md and gate | M5 | todo | |
| T-41 | Telemetry and dashboard | M6 | todo | |
| T-42 | Security controls and threat model | M6 | todo | |
| T-43 | Images, compose profiles, test compose | M7 | todo | |
| T-44 | Terraform module | M7 | todo | |
| T-45 | CI workflows | M7 | todo | |
| T-46 | Demo scenario | M8 | todo | |
| T-47 | Documentation set | M8 | todo | |
| T-48 | Durability and chaos | M9 | todo | |
| T-49 | Reviews and fixes | M9 | todo | |
| T-50 | Final verification | M9 | todo | |

## Global Definition of Done

- [ ] `make verify` passes from a fresh clone (§19) with no API keys set.
- [ ] `docker compose up -d --wait` brings every service to healthy; `make seed` and `make demo` run the scripted scenario end to end.
- [ ] `make test-docker` runs the full test suite inside containers and passes.
- [ ] Unit, integration, contract and E2E suites pass; line coverage is at least 85% on core packages and 70% overall.
- [ ] Offline evals meet every threshold in §11, and `evals/REPORT.md` is generated by script from the latest run.
- [ ] Lint, format and type checks are clean.
- [ ] Security scans report no high or critical findings, or each one is documented with a reason in `docs/security.md`.
- [ ] Every workflow in §17 exists and passes actionlint; if a GitHub remote exists and CI results can be observed, the pushed branch is green.
- [ ] Every screen in §13 is implemented with loading, empty, error and success states; axe reports no serious violations; Playwright captured the screenshots in `docs/images/`.
- [ ] The harness in §7 is complete: CLAUDE.md, AGENTS.md, settings, hooks with passing tests, rules, subagents, skills and `.mcp.json`.
- [ ] Every document in §18 exists, and every number in the README points to the command or report that produced it.
- [ ] The `reviewer` subagent's final verdict is "ship", and `security-review` findings are fixed or documented.
- [ ] `PROGRESS.md` has every task checked with evidence, and `docs/KNOWN_LIMITATIONS.md` lists what is not done.
- [ ] Every commit is authored and committed by the maintainer, and no commit message or pull request contains `Co-Authored-By`, `Claude-Session` or "Generated with Claude Code" (checked as in §19).
- [ ] `BUILD.md` lives at `docs/specs/000-build-spec.md`; all work is committed, and pushed when a remote exists.
- [ ] FR-1 to FR-13 each have a passing named test in `PROGRESS.md`.
- [ ] `make bench` p95 < 80 ms at 300 RPS; the laptop run (2,000 RPS) is reported as measured.
- [ ] §11 thresholds met: recall@1%FPR ≥ 0.80 (rules-only baseline reported), faithfulness ≥ 0.95, PII to provider 0.
- [ ] `make feature-parity` 0 mismatches; `TestDurabilityKill` 0 lost decisions; `make tf-check` passes without AWS credentials; Triton is never required by `make verify`.

## Functional requirement tests

| FR | Named test | Status |
|---|---|---|
| FR-1 | | todo |
| FR-2 | | todo |
| FR-3 | | todo |
| FR-4 | | todo |
| FR-5 | | todo |
| FR-6 | | todo |
| FR-7 | | todo |
| FR-8 | | todo |
| FR-9 | | todo |
| FR-10 | | todo |
| FR-11 | | todo |
| FR-12 | | todo |
| FR-13 | | todo |

## Milestone reviews

| Milestone | Reviewer verdict | Notes |
|---|---|---|
| M0 | fix first → fixed | Blockers: `go vet ./...` parsed `Dockerfile.go` (fixed with `GO_PKGS`, regression test `tests/harness/test_go_packages.py`); settings deny `Read(./.env.*)` also blocked `.env.example` (replaced with explicit patterns). Non-blocking fixed: skeleton rejects bad `amount_minor`; CLAUDE.md lists every target. |
