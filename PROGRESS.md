# PROGRESS

Build progress for `fraud-scoring-service`, driven by `docs/specs/000-build-spec.md` (originally `BUILD.md`).

## Needs the maintainer

- **evalgate tag missing.** `git ls-remote https://github.com/ROBERTG99/evalgate` on 2026-10-16 shows only `refs/heads/main` (no `v0.1.0` tag, no code yet). Per §11 the consumer dependency rule applies: a minimal report.v1-compatible implementation lives in `third_party/evalgate/`. When `v0.1.0` is published, switch `pyproject.toml` to `evalgate @ git+https://github.com/ROBERTG99/evalgate@v0.1.0`, point `.github/workflows/evals.yml` at `ROBERTG99/evalgate/action@v0.1.0`, and delete `third_party/evalgate/`.
- **No LLM provider key in the build environment.** `ANTHROPIC_API_KEY` and `OPENAI_API_KEY` are unset, so
  LLM adapter fixtures are hand-authored in the documented shapes (D-011) and `make eval-live` /
  `make record-fixtures` cannot run here. Set the key in the cloud environment's variables to record real
  cassettes and run live evals.
- **Confirm the OpenAI default model.** `FRAUD_OPENAI_MODEL` defaults to `gpt-6-luna` (D-010); the OpenAI docs
  host was unreachable for a full read. Override the variable if your account uses a different model.
- **Maintainer review of 30 judge labels** (spec §11) — pending until the judge calibration set exists (T-38).

## Session handoff (read this first)

Last updated 2026-10-16 at the end of session 1. Status: **M0, M1, M2 done and reviewed; M3 in progress
(T-23, T-24 done; next is T-25).** Branch `feat/friendly-johnson-15rki1`, everything committed and pushed.

### Resume checklist

1. Re-read `docs/specs/000-build-spec.md` (identical to `BUILD.md`), this file, `docs/DECISIONS.md`,
   `git log --oneline -30`.
2. Environment (Claude Code on the web; `.claude/hooks/session_start.sh` does most of this):
   - Docker: `mkdir -p /etc/docker && echo '{"registry-mirrors":["https://mirror.gcr.io"]}' > /etc/docker/daemon.json && (nohup dockerd > /tmp/dockerd.log 2>&1 &)`.
   - Tools: `make bootstrap` (idempotent). Shells in this environment do not source `~/.bashrc`, so prefix
     commands with `export PATH=$PATH:/root/go/bin:$PWD/.cache/bin:/root/.local/bin GOTOOLCHAIN=local`.
   - Test infrastructure: `docker compose -f compose.test.yaml -p fraudtest up -d --wait` (Postgres 8521,
     Redis 8531, Redpanda 8543). Go integration tests also fall back to testcontainers / a local
     `redis-server`; set `FRAUD_TEST_PG_DSN=postgres://fraud:fraud@127.0.0.1:8521/fraud?sslmode=disable` to
     use compose.
   - Use `set -o pipefail` when piping `make check-fast | tail`: an earlier commit slipped through a red run
     because `tail` masked the exit code (fixed by amending before push).
   - The PreToolUse guard (`.claude/hooks/pre_tool_guard.py`) is active in-session: never put
     `rm -rf <path>` text and unrelated paths in one command, and never `pkill -f` a pattern that matches the
     running shell (it kills the shell).
3. Data/model artifacts are git-ignored; regenerate with `make gen-data SIZE=demo` (3 s), `make train` (11 s,
   registers `fraud-gbdt` in `mlruns/` SQLite), `make register-model ALIAS=production` and `ALIAS=shadow`,
   `make gen-data SIZE=eval` (34 s, 838 MB) when evals need it.

### Next steps (in order)

- **T-25 group commit + spool** (`internal/store/{writer,spool}.go`): implement `api.DecisionStore`
  (`Commit` returns the stored decision; `INSERT … ON CONFLICT (transaction_id) DO NOTHING` then fetch the
  existing row — D-017), one transaction for `decisions` + `decision_outbox` + `review_cases` (REVIEW only),
  batches every 2 ms or 256 rows from a 10,000-slot queue (full for 5 ms → `RESOURCE_EXHAUSTED`), Postgres
  error or > 30 ms → append to the spool in `FRAUD_SPOOL_DIR`, replay on recovery, `fraud_spool_depth`.
  Tests: `TestCommitBeforeRespond`, `TestSpoolReplayAfterPostgresRecovers`; also `GetByTransaction`.
- T-26 outbox relay (`SELECT … FOR UPDATE SKIP LOCKED`, produce, mark `sent_at`; kfake crash test), T-27
  shadow (`internal/model/shadow.go` 16 workers / 2,000 queue / drop counter; `model.ShadowJob` already
  exists; `internal/store/shadow.go`), T-28 CaseService + RBAC + dev-issuer (the auth interceptor in
  `internal/api/interceptors.go` already takes a `TokenVerifier` and an `Authorizer`), T-29 summarizer
  consumer + Postgres repository (implements `CaseRepository` in `src/fraud_summarizer/pipeline.py`:
  `load_case`, `save_summary`, `save_steps`), T-30 SageMaker launcher (boto3 only, D-003), T-31 seed/replay.
- `cmd/scorer/main.go` still runs the T-05 HTTP skeleton (`cmd/scorer/skeleton.go`); wire the real gRPC
  server + grpc-gateway (use `runtime.WithMarshalerOption` with `UseProtoNames: true`, matching
  `json_names_for_fields=false` in `buf.gen.yaml`) when T-25..T-28 land, and keep
  `TestScoreReturnsRulesOnlyDecision` meaningful or replace it with an equivalent end-to-end test.
- Dockerfile targets for `feature-updater`, `dev-issuer`, `webserve`, `seed`, `retention` and compose
  services are still to be added (T-43); `Dockerfile.go` currently builds only `scorer`.

### Deferred review items (not blockers; pick up in T-42 / T-49)

- Redaction misses unseparated 10-digit phone numbers, dot-separated card numbers and IBANs
  (`src/fraud_summarizer/redact.py`; document or extend in T-42).
- The case-summary system prompt is shorter than the minimum cacheable prefix, so `cache_control` is a no-op
  today; harmless, note it in `docs/evaluation.md` cost section.
- `training/evaluate.py` (T-37) must also report test recall at the threshold chosen on valid, not only the
  test-optimal operating point.
- The injection test is partly tautological with FakeLLM (it does prove the fencing); add a live/cassette
  variant in `make eval-live` when a key exists.
- Dev database roles keep `LOGIN` with dev passwords (`migrations/00001_roles.sql`); document in
  `docs/security.md` (T-42).
- `internal/rules/watch_test.go` polls with short sleeps inside a deadline loop (acceptable per §10, keep).

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
| M1 | fix first → fixed | Blockers fixed: consumer `Run` could hang on shutdown while a handler kept failing (select on send; regression `TestRunReturnsWhileHandlerKeepsFailing` fails without the fix); updater Redis client timed out pipelines (dedicated `NewUpdaterRedisClient`, 500-command chunks, benchmark rerun with it: 57,359 events/s); credential reads made exact (16-entry history per HASH field, exact `minutes_since` assertion) and `docs/KNOWN_LIMITATIONS.md` added. Non-blocking fixed: DLQ no longer duplicated on retries (`TestRetriedBatchDoesNotDuplicateDLQ`), insert-failure metric, `audit_log.prev_hash` UNIQUE, per-account timestamp assertion in offline features. Deferred: dev roles keep LOGIN with dev passwords (documented in `docs/security.md`, T-42). |
| M2 | fix first → fixed | Blockers fixed: `provider_schema` dropped the `pattern` property from provider schemas (now strips only constraint keywords; `test_provider_schema_keeps_every_property`); faithfulness regex missed numbers with units such as `12x`, `30min`, `.5` (`test_numbers_with_units_are_checked`); gateway retried invalid JSON/schema output against Appendix B (now non-retryable, moves to the fallback provider — D-014). Non-blocking fixed: BentoML request-buffer pool race removed, default 20 ms deadline in model clients, expr builtins restricted + rule runtime-error metric + file/YAML version check + OnLoad lock (D-016), cache-aware cost (D-015), facts fence escaping, cassette `recorded` defaults to false, SSN and date/time redaction false positives, both-down floor recorded (D-012). ML review: split, calibration, ONNX parity and D-008 judged sound. Deferred items listed under Session handoff. |
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
| T-12 | Feature-updater and DLQ | M1 | done | `go test -race ./cmd/feature-updater` (TestUpdaterRoutesInvalidToDLQ, TestRetriedBatchDoesNotDuplicateDLQ, kfake) → ok; `go test -tags integration -bench BenchmarkUpdater -benchtime 3x ./cmd/feature-updater` with the production NewUpdaterRedisClient → 57,359 events/s (budget ≥ 10,000) |
| T-13 | Train/serve feature parity | M1 | done | `make feature-parity` → 3430 transactions x 13 features, 0 mismatches (Go TestOnlineOfflineParity vs training/features.py) |
| T-14 | Training, calibration, ONNX export | M2 | done | `uv run pytest tests/unit/test_training.py` (test_temporal_split_has_no_overlap, test_onnx_matches_native_within_1e5) → 4 passed; demo train 4.6 s, test PR-AUC 0.9107, recall@1%FPR 0.9080, ECE 0.0007, ONNX max diff 5.66e-06 |
| T-15 | MLflow registry | M2 | done | `uv run pytest tests/unit/test_registry.py::test_set_alias_production` (SQLite store, D-009) → pass; `make train` → 11.0 s, registered fraud-gbdt v1 with pr_auc and recall_at_1pct_fpr tags; `make register-model ALIAS=production` → v1 @production |
| T-16 | BentoML model server | M2 | done | `uv run pytest tests/unit/test_model_server.py::test_predict_golden_vector` → pass (5 golden vectors within 1e-6, HTTP /predict via ASGI); `make bench-model` → p50 2.99 ms, p95 4.63 ms, p99 6.65 ms over 2,000 single-row requests (budget p95 < 8 ms) |
| T-17 | Go model client | M2 | done | `go test -race ./internal/model` (TestClientTimeoutReturnsErrUnavailable: 50 ms server, 20 ms deadline → ErrUnavailable < 30 ms) → ok; `go test -tags integration -run TestGoldenVectorScore ./internal/model` (real BentoML, 5 golden vectors, both aliases, within 1e-5) → ok |
| T-18 | Triton profile and KServe v2 client | M2 | done | `go test ./internal/model -run TestKServeV2RequestShape` (recorded KServe v2 request/response) → ok; Triton profile test skipped: nvcr.io blocked (Forbidden), recorded in docs/KNOWN_LIMITATIONS.md |
| T-19 | Rules engine | M2 | done | `go test -race ./internal/rules` (TestInvalidExprKeepsPreviousVersion, schema/limits, watch reload) → ok; `go test -bench BenchmarkEval64Rules ./internal/rules` → 14.9 µs per 64-rule evaluation (budget < 50 µs) |
| T-20 | Combination table | M2 | done | `go test -race ./internal/decide` (TestFinalRuleWins, TestDegradedBandByAmount, TestRuleR003WithLowScoreReviews, rapid TestActionMonotonicInScore) → ok, coverage 96.7% |
| T-21 | LLM gateway and FakeLLM | M2 | done | `uv run pytest tests/llm` under the socket guard (TCP disabled, no loopback) → 10 passed incl. test_retries_only_retryable_codes and test_adapters_against_cassettes (Anthropic Messages + OpenAI Responses documented-shape fixtures, D-011) |
| T-22 | Summary pipeline and faithfulness | M2 | done | `uv run pytest tests/unit/test_pipeline.py` (test_number_mismatch_drops_item, test_injection_in_merchant_name_is_ignored, Hypothesis absent-key rejection, fallback after 3 failures) → 10 passed; `tests/unit/test_redact.py` → 14 passed |
| T-23 | Score handler with idempotency | M3 | done | `go test -race ./internal/api` (TestScoreReplayReturnsSameDecisionID, TestMissingAmountInvalidArgument, concurrent duplicates share one decision, Postgres idempotency fallback, service-token auth) → ok; `TestRedisIdempotencyFirstWriteWins` (integration) → ok |
| T-24 | Degradation paths | M3 | done | `go test -race -count=3 ./internal/api` (TestModelTimeoutRulesOnlyWithin35ms: 50 ms BentoML stub → < 35 ms, MODEL_UNAVAILABLE, model_version none; TestRedisDownBiasesToReview: FEATURES_UNAVAILABLE, REVIEW from 5,000 minor) → ok |
| T-25 | Group commit and spool | M3 | done | `go test -race -tags integration ./internal/store -run "TestCommitBeforeRespond\|TestGroupCommitBatchesConcurrentWrites\|TestSpoolReplayAfterPostgresRecovers"` → ok (500 concurrent commits batched; 20 decisions spooled during a proxy-cut outage replayed once each, outbox and cases included); queue-full → RESOURCE_EXHAUSTED |
| T-26 | Outbox relay | M3 | done | `go test -race -tags integration ./internal/store -run TestRelayCrashProducesDuplicatesWithSameDecisionID` (kfake) → ok: crash after produce leaves 5 rows unsent, restart re-sends, 10 messages for 5 decision_ids, consumer dedupe processes 5 |
| T-27 | Shadow mode | M3 | done | `go test -race ./internal/api -run TestShadowFailureDoesNotChangeDecisions` (1,000 decisions identical with an always-failing shadow; all three actions covered) → ok; `internal/model/shadow_test.go` (drops counted in fraud_shadow_dropped_total, Submit never blocks) → ok; `TestShadowStoreInsert` → ok |
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
| FR-1 | `internal/api/scorer_test.go::TestScoreReplayReturnsSameDecisionID` (+ `TestDecisionFieldsStable`) | pass (unit); end-to-end gRPC/gateway pending T-28 |
| FR-2 | `internal/stream/validate_test.go::TestInvalidEventRejected`, `cmd/feature-updater/updater_test.go::TestUpdaterRoutesInvalidToDLQ` | pass |
| FR-3 | `tests/integration/parity_test.go::TestOnlineOfflineParity` (0 mismatches), `internal/features/redis_test.go::TestOutOfOrderWithinFiveMinutes` | pass |
| FR-4 | `cmd/gen/gen_test.go::TestDeterministicSHA` | pass |
| FR-5 | `make train` (11.0 s, registers fraud-gbdt with pr_auc and recall_at_1pct_fpr), `tests/unit/test_registry.py::test_set_alias_production` | pass |
| FR-6 | `internal/model/golden_test.go::TestGoldenVectorScore` (BentoML, within 1e-5); Triton skipped — `docs/KNOWN_LIMITATIONS.md` | pass / Triton skip recorded |
| FR-7 | `internal/decide/combine_test.go::TestRuleR003WithLowScoreReviews` | pass |
| FR-8 | `internal/api/degrade_test.go::TestModelTimeoutRulesOnlyWithin35ms`, `::TestRedisDownBiasesToReview` | pass; Postgres-down spool pending T-25 |
| FR-9 |  | todo (T-27) |
| FR-10 |  | todo (T-25, T-26) |
| FR-11 | `tests/unit/test_pipeline.py::test_happy_path_stores_verified_summary_and_steps`, `::test_provider_failures_write_fallback_summary` | pass (pipeline); consumer + DB pending T-29 |
| FR-12 |  | todo (T-30, T-44) |
| FR-13 |  | todo (T-28, T-32..T-35) |

## Milestone reviews

| Milestone | Reviewer verdict | Notes |
|---|---|---|
| M0 | fix first → fixed | Blockers: `go vet ./...` parsed `Dockerfile.go` (fixed with `GO_PKGS`, regression test `tests/harness/test_go_packages.py`); settings deny `Read(./.env.*)` also blocked `.env.example` (replaced with explicit patterns). Non-blocking fixed: skeleton rejects bad `amount_minor`; CLAUDE.md lists every target. |
