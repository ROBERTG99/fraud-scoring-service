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

Last updated 2026-10-17. Status: **M0–M2 done and reviewed; M3 code complete (T-23..T-31 done) and the M3
review blockers fixed (hung-Postgres breaker, replay leak), M3 re-review pending; M4 started (T-32 done).**
Data is generator v2 with feature spec v2 (D-026). Branch `feat/friendly-johnson-15rki1`, pushed.

### Resume checklist

1. Re-read `docs/specs/000-build-spec.md` (identical to `BUILD.md`), this file, `docs/DECISIONS.md`,
   `git log --oneline -30`.
2. Environment (Claude Code on the web; `.claude/hooks/session_start.sh` does most of this):
   - Docker: `mkdir -p /etc/docker && echo '{"registry-mirrors":["https://mirror.gcr.io"]}' > /etc/docker/daemon.json && (nohup dockerd > /tmp/dockerd.log 2>&1 &)`.
     The worker can restart and take dockerd down: check `docker info` and restart it plus the test stack.
   - Python image builds need the proxy CA: `docker build --network host --secret id=extra_ca,src=/root/.ccr/ca-bundle.crt ...` (D-023).
   - Tools: `make bootstrap` (idempotent). Shells in this environment do not source `~/.bashrc`, so prefix
     commands with `export PATH=$PATH:/root/go/bin:$PWD/.cache/bin:/root/.local/bin GOTOOLCHAIN=local`.
   - Test infrastructure: `docker compose -f compose.test.yaml -p fraudtest up -d --wait` (Postgres 8521,
     Redis 8531, Redpanda 8543). Integration env: `FRAUD_TEST_PG_DSN=postgres://fraud:fraud@127.0.0.1:8521/fraud?sslmode=disable`,
     `FRAUD_TEST_REDIS_ADDR=127.0.0.1:8531`, `FRAUD_TEST_KAFKA_BROKERS=127.0.0.1:8543`.
   - Use `set -o pipefail` when piping make targets, and do not chain the test run after a lint step with
     `&&` (a gofmt failure silently skipped the tests once).
   - The PreToolUse guard (`.claude/hooks/pre_tool_guard.py`) is active in-session: never put
     `rm -rf <path>` text and unrelated paths in one command, and never `pkill -f` a pattern that matches the
     running shell (it kills the shell).
3. Data/model artifacts are git-ignored; regenerate with `go run ./cmd/gen -size demo -seed 7 -out artifacts/data/demo`,
   `python -m training.train --data artifacts/data/demo --out artifacts/model` (registers `fraud-gbdt`).
   End-to-end seed check on the host: `scripts/seed_host.sh` (stop its services afterwards, see its header).

### Next steps (in order)

- M3 re-review (reviewer subagent) of the fixes since the first M3 review, then push and record the M3 row.
- M4: T-33 queue and case detail (`web/src/routes/{queue,case}.tsx`, `AiSummary`, `LabelDialog` with
  `LabelDialog.test.tsx`), T-34 timeline/shadow/live/rules (`live.test.tsx` reconnect), T-35 e2e + axe +
  screenshots. The console talks to `/api` (gateway), `/auth` (dev-issuer) and `/summarizer` via webserve.
- Reviewer non-blocking items still open: gRPC health check rejected by the auth interceptor; ShadowRunner
  `Submit` after `Close` panics; JWKS lock held across the fetch (Go and Python); concurrent labels with the
  same idempotency key on different cases return 500; regenerate idempotency key is global and has no rate
  limit; `ShadowSummary` uses `time.Now()`; one bad spooled row blocks the spool drain; malformed spool lines
  are skipped silently; `sagemaker_entry` ignores the `seed` hyperparameter.
- Dockerfile targets for `feature-updater`, `dev-issuer`, `webserve`, `seed`, `retention` and compose
  services (T-43); compose must pass the `extra_ca` build secret.

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
| T-02 | Makefile and toolchains | M0 | done | `make bootstrap` → done in 35 s; `make check-fast` → exit 0 (go test ok, 41 pytest passed); `test_all_block_h_targets_declared` passes |
| T-03 | Harness docs, rules, subagents, skills | M0 | done | harness files exist; `uv run pytest tests/harness/test_harness_docs.py` passes; reviewer ran on M0 (see Milestone reviews) |
| T-04 | Hooks with tests | M0 | done | `uv run pytest tests/harness/test_hooks.py` → 35 passed (force push, test-partition edit, attribution, identity, terraform, rm -rf, .env); session_start.sh run with CLAUDE_CODE_REMOTE=true → rc 0, idempotent |
| T-05 | Walking skeleton in Docker | M0 | done | `go test ./cmd/scorer -run TestScoreReturnsRulesOnlyDecision` → ok; `docker compose up -d --build --wait` → scorer + case-summarizer healthy (41 s); `curl localhost:8511/healthz` → 200; `make check-fast` → exit 0 after M0 review fixes |
| T-06 | Protobuf and codegen | M1 | done | `./scripts/proto.sh` twice → identical `gen/` (diff empty); `buf lint` clean; `go test ./tests/contract -run TestDecisionFieldsStable` → ok |
| T-07 | Event schemas and validators | M1 | done | `go test ./internal/stream` (TestInvalidEventRejected, TestSharedFixtures 40 fixtures) → ok; `uv run pytest tests/unit/test_events.py::test_shared_fixtures_agree` → pass (20 valid, 20 invalid) |
| T-08 | Synthetic generator | M1 | done | GenVersion 2 (D-026: look-alikes + low-and-slow fraud). `go test ./cmd/gen` (TestDeterministicSHA, TestFraudRateWithinTolerance, schema/order/split tests) → ok; `go run ./cmd/gen -size demo -seed 7` → 200,000 events in 1.5 s, 135,005 transactions, fraud rate 0.0079, sha256 e59f1f202eff (v1 was ccd80914a827) |
| T-09 | Migrations | M1 | done | `go test -tags integration ./internal/store` (TestUpDownUp, TestAuditLogRejectsUpdate, TestDecisionConstraints) → ok on Postgres 16 (testcontainers and compose.test.yaml) |
| T-10 | Feature spec and offline features | M1 | done | Feature spec v2 (new_device = first seen < 24 h ago, D-026). `uv run pytest tests/unit/test_features.py` (Hypothesis point-in-time + vectorized vs naive, test_device_is_known_24h_after_first_sighting) → pass |
| T-11 | Redis Lua feature store | M1 | done | `go test -race -tags integration ./internal/features/` (TestReadAsOfThenAddIsAtomic: 1 round trip per ScoreTx, 64 concurrent; TestOutOfOrderWithinFiveMinutes rapid 100 cases) → ok |
| T-12 | Feature-updater and DLQ | M1 | done | `go test -race ./cmd/feature-updater` (TestUpdaterRoutesInvalidToDLQ, TestRetriedBatchDoesNotDuplicateDLQ, kfake) → ok; `go test -tags integration -bench BenchmarkUpdater -benchtime 3x ./cmd/feature-updater` with the production NewUpdaterRedisClient → 57,359 events/s (budget ≥ 10,000) |
| T-13 | Train/serve feature parity | M1 | done | `make feature-parity` (spec v2, parity fixture regenerated with gen v2) → 3502 transactions x 13 features, 0 mismatches (Go TestOnlineOfflineParity vs training/features.py) |
| T-14 | Training, calibration, ONNX export | M2 | done | `uv run pytest tests/unit/test_training.py` (test_temporal_split_has_no_overlap, test_onnx_matches_native_within_1e5) → pass; gen v2 demo: `python -m training.train --data artifacts/data/demo --out artifacts/model --update-spec` → 5.3 s, test PR-AUC 0.9201, recall@1%FPR 0.9651, ECE 0.0009, ONNX max diff 7.14e-06, registered fraud-gbdt v3 (v1 data: PR-AUC 0.9107, recall 0.9080) |
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
| T-28 | CaseService, RBAC, dev-issuer | M3 | done | internal/api/cases.go (CaseServer), internal/auth (EdDSA JWT, JWKS, RBAC), cmd/dev-issuer; TestAnalystCannotTestRule (internal/api, real interceptors + JWTs over bufconn) and TestLabelWritesAuditRow (internal/store) pass; cmd/scorer serves gRPC :8510 + gateway :8511 (TestScoreReturnsRulesOnlyDecision, TestGatewayAuthAndLimits); tests/contract/test_openapi_contract.py: Schemathesis passes on all 8 non-streaming operations (3 consecutive runs, fresh DB each) |
| T-29 | Summarizer consumer | M3 | done | src/fraud_summarizer/{consumer,app,repository,auth,metrics}.py; test_duplicate_decision_processed_once, test_provider_failure_writes_fallback pass; tests/integration/test_summarizer_consumer.py writes an ok account_takeover summary for a REVIEW decision from Redpanda into Postgres (duplicate skipped, invalid message to DLQ; 3 consecutive runs); make test-py: 172 passed, src/fraud_summarizer 93.6% (scripts/pycover.py) |
| T-30 | SageMaker launcher | M3 | done | training/sagemaker_launch.py (boto3 only, request validated offline against botocore service model), training/sagemaker_entry.py, training/Dockerfile; test_dry_run_request_shape, test_submit_to_moto pass; `make sagemaker-dry-run` exit 0 with AWS_* unset; image built (`docker build --network host --secret id=extra_ca,...`) and trained offline with `docker run --network none` (tiny: model.onnx + model_meta.json written) |
| T-31 | Seed job and replay | M3 | done | internal/replay (warm via Kafka + wait; live per-account apply-then-score, D-025), cmd/replay, cmd/seed, internal/model/batcher.go; TestReplayRateLimit, TestSeedIdempotent, TestReplayDoesNotLeakLaterEventsIntoFeatures pass. On gen v2 data + spec v2 model (D-026): `scripts/seed_host.sh` → seed exit 0 in 74 s, 16,904 scored, 0 errors, 74 open cases (≥ 50; REVIEW: 31 account takeover, 14 mule, 26 legitimate look-alikes, 3 other), 59 degraded, Score p50 3.5 / p95 8.6 / p99 12.7 ms. v1 numbers (144/148 cases) came from a leaking replay and are withdrawn |
| T-32 | Console scaffold and data layer | M4 | done | web/ (Vite 7 + React 19 + TanStack Query 5 + Tailwind 4; generated web/src/api/schema.d.ts; api/client.ts with 401 → clear session + /sign-in?next=; auth/session.ts; tokens.css with text-safe status tokens), cmd/webserve (SPA + CSP + /api,/auth,/summarizer proxies, unbuffered streaming); client.test.ts "401 clears the session and redirects to sign-in" passes; `pnpm -C web test` 32 passed incl. WCAG contrast checks; `pnpm -C web build` ok; go test -race ./cmd/webserve ok |
| T-33 | Queue and case detail | M4 | done | web/src/routes/{queue,case}.tsx, components/{AiSummary,LabelDialog,ReasonChips}.tsx; LabelDialog.test.tsx "previews the exact row to be written and sends the same values" passes; queue/case tests cover loading, empty, error-with-retry and success, keyboard navigation (j/k/Enter), evidence → fact highlight, role-gated Label/Regenerate; `pnpm -C web test` 41 passed (3 consecutive runs), `pnpm -C web build` ok. Summarizer regenerate now typed (gen/openapi/summarizer.json → web/src/api/summarizer.d.ts), idempotency key scoped per case, 20/hour per user |
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
| FR-1 | `internal/api/scorer_test.go::TestScoreReplayReturnsSameDecisionID` (+ `TestDecisionFieldsStable`) | pass (unit + gateway `cmd/scorer` + Schemathesis) |
| FR-2 | `internal/stream/validate_test.go::TestInvalidEventRejected`, `cmd/feature-updater/updater_test.go::TestUpdaterRoutesInvalidToDLQ` | pass |
| FR-3 | `tests/integration/parity_test.go::TestOnlineOfflineParity` (0 mismatches), `internal/features/redis_test.go::TestOutOfOrderWithinFiveMinutes` | pass |
| FR-4 | `cmd/gen/gen_test.go::TestDeterministicSHA` | pass |
| FR-5 | `make train` (11.0 s, registers fraud-gbdt with pr_auc and recall_at_1pct_fpr), `tests/unit/test_registry.py::test_set_alias_production` | pass |
| FR-6 | `internal/model/golden_test.go::TestGoldenVectorScore` (BentoML, within 1e-5); Triton skipped — `docs/KNOWN_LIMITATIONS.md` | pass / Triton skip recorded |
| FR-7 | `internal/decide/combine_test.go::TestRuleR003WithLowScoreReviews` | pass |
| FR-8 | `internal/api/degrade_test.go::TestModelTimeoutRulesOnlyWithin35ms`, `::TestRedisDownBiasesToReview`, `internal/store/writer_test.go::TestSpoolReplayAfterPostgresRecovers`, `::TestHungPostgresSpoolsInsteadOfOverloading` | pass |
| FR-9 | `internal/api/shadow_test.go::TestShadowFailureDoesNotChangeDecisions` | pass (console screen pending T-34) |
| FR-10 | `internal/store/writer_test.go::TestCommitBeforeRespond`, `internal/store/outbox_test.go::TestRelayCrashProducesDuplicatesWithSameDecisionID` | pass |
| FR-11 | `tests/unit/test_pipeline.py::test_happy_path_stores_verified_summary_and_steps`, `tests/unit/test_consumer.py::test_duplicate_decision_processed_once`, `::test_provider_failure_writes_fallback`, `tests/integration/test_summarizer_consumer.py::test_consumer_writes_summary_for_review_event` | pass (pipeline, consumer, Postgres + Redpanda) |
| FR-12 | `tests/unit/test_sagemaker_launch.py::test_dry_run_request_shape`, `::test_submit_to_moto` | launcher pass; Terraform pending T-44 |
| FR-13 | `internal/api/cases_test.go::TestAnalystCannotTestRule`, `internal/store::TestLabelWritesAuditRow`, `tests/contract/test_openapi_contract.py` | API pass; console pending T-32..T-35 |

## Milestone reviews

| Milestone | Reviewer verdict | Notes |
|---|---|---|
| M0 | fix first → fixed | Blockers: `go vet ./...` parsed `Dockerfile.go` (fixed with `GO_PKGS`, regression test `tests/harness/test_go_packages.py`); settings deny `Read(./.env.*)` also blocked `.env.example` (replaced with explicit patterns). Non-blocking fixed: skeleton rejects bad `amount_minor`; CLAUDE.md lists every target. |
| M1 | fix first → fixed | Blockers fixed: consumer `Run` could hang on shutdown while a handler kept failing (select on send; regression `TestRunReturnsWhileHandlerKeepsFailing` fails without the fix); updater Redis client timed out pipelines (dedicated `NewUpdaterRedisClient`, 500-command chunks, benchmark rerun with it: 57,359 events/s); credential reads made exact (16-entry history per HASH field, exact `minutes_since` assertion) and `docs/KNOWN_LIMITATIONS.md` added. Non-blocking fixed: DLQ no longer duplicated on retries (`TestRetriedBatchDoesNotDuplicateDLQ`), insert-failure metric, `audit_log.prev_hash` UNIQUE, per-account timestamp assertion in offline features. Deferred: dev roles keep LOGIN with dev passwords (documented in `docs/security.md`, T-42). |
| M2 | fix first → fixed | Blockers fixed: `provider_schema` dropped the `pattern` property from provider schemas (now strips only constraint keywords; `test_provider_schema_keeps_every_property`); faithfulness regex missed numbers with units such as `12x`, `30min`, `.5` (`test_numbers_with_units_are_checked`); gateway retried invalid JSON/schema output against Appendix B (now non-retryable, moves to the fallback provider — D-014). Non-blocking fixed: BentoML request-buffer pool race removed, default 20 ms deadline in model clients, expr builtins restricted + rule runtime-error metric + file/YAML version check + OnLoad lock (D-016), cache-aware cost (D-015), facts fence escaping, cassette `recorded` defaults to false, SSN and date/time redaction false positives, both-down floor recorded (D-012). ML review: split, calibration, ONNX parity and D-008 judged sound. Deferred items listed under Session handoff. |
