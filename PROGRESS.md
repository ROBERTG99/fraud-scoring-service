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
- **Maintainer review of 30 judge labels** (spec §11): the set exists (`evals/datasets/judge_calibration/`, 80
  items labelled by construction; offline judge agreement 0.900 on test). Please review 30 of them (for example
  the first 15 of each split) and note disagreements; a live judge score counts only after `make eval-live`
  reports agreement >= 0.8.

- **govulncheck needs `vuln.go.dev`.** The build environment's network policy denies it, so `make security`
  reports govulncheck as not run. Allow the host in the environment's network settings (or run it in CI).

## Session handoff (read this first)

Last updated 2026-10-18. Status: **M0–M5 done and reviewed. M6: T-41 and T-42 done; every finding of the
M6 security audit (H1–H3, M1–M9, L1–L11) and of all five review rounds is fixed (D-041 to D-065); round 6 of the
M6 review (reviewer, security-auditor) is next.** `make lint` stops at `lint-tf` until T-44 adds
`infra/terraform` and `scripts/terraform.sh`; run `lint-go lint-py lint-web lint-proto lint-md` meanwhile.
Data is generator v2 with feature spec v2 (D-026). Branch `feat/friendly-johnson-15rki1`, pushed.

### Resume checklist

1. Re-read `docs/specs/000-build-spec.md` (identical to `BUILD.md`), this file, `docs/DECISIONS.md`,
   `git log --oneline -30`.
2. Environment (Claude Code on the web; `.claude/hooks/session_start.sh` does most of this):
   - Docker: `mkdir -p /etc/docker && echo '{"registry-mirrors":["https://mirror.gcr.io"]}' > /etc/docker/daemon.json && (nohup dockerd > /tmp/dockerd.log 2>&1 &)`.
     The worker can restart and take dockerd down: check `docker info` and restart it plus the test stack.
   - Python image builds need the proxy CA: `docker build --network host --secret id=extra_ca,src=/root/.ccr/ca-bundle.crt ...` (D-023).
   - Tools: `make bootstrap` (idempotent). Shells in this environment do not source `~/.bashrc`, so prefix
     commands with `export PATH=$PATH:/root/go/bin:$PWD/.cache/bin:/root/.local/bin GOTOOLCHAIN=auto` (go.mod
     pins `toolchain go1.26.9`, fetched once from the Go module proxy; D-042).
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

- M4 is closed (see Milestone reviews). Open, non-blocking console items from the M4 reviews: focus drops to
  `<body>` when the focused queue row is labelled elsewhere (move it to the row that takes its place); keep the
  last case in the URL so returning from a case restores the queue row; Home/End in the queue; "Back to
  evidence" after an evidence jump; Live "Retry now" and p95-over-budget flag; 404 "not found" + link instead of
  Retry; header wrap and `whitespace-nowrap` amounts at 375 px; active nav non-colour cue; per-route page title;
  shadow matrix padding and thousands separators; light placeholder contrast; `score=0` on approvals and raw
  `decided_at` in the timeline; label preview should say `label_id`/`created_at` are server-set; buttons could
  use `border-field-border`. Code: summarizer consumer retries a deterministic pipeline bug forever on one
  partition (`src/fraud_summarizer/consumer.py:164`; add a retry cap → DLQ in T-42/T-48 or document);
  `TestCompletedComparisonsAreNotCountedAsDropped` does not cover the `cmd/scorer/main.go` wiring;
  `shadow.png` compares v3 with v3 (register a distinct shadow version before recapturing, T-46);
  `docs/security.md` (T-42).
- M5 (next: T-36 eval datasets).
- M5 facts:
  - `third_party/evalgate/` now implements `evalgate run` and `evalgate check` with upstream's contracts
    (D-030; `tests/unit/test_evalgate.py`). Still missing: `evalgate calibrate` (judge agreement, T-38). Suites
    are `evalgate.suite.v1` YAML with `target: {python: module:fn}`, `metrics` (`exact` with
    `params: {actual: output.x, expected: expected.x}` or `custom` registered with `@evalgate.metric`, modules
    listed under `plugins`), dataset path relative to the suite file. `check` fails closed on reports without
    per-case scores (`cases: []`) and on < 10 paired cases against a baseline, so `training/evaluate.py` and
    `cmd/benchreport` must emit per-case scores too. `evals/reports/model_latency.json` has `cases: []`.
  - T-36 plan (`scripts/gen_eval_sets.py`, seed 7, `random.Random(f"7/<set>")`, sorted-key JSONL; test first
    `tests/unit/test_eval_sets.py::test_splits_disjoint_and_counts` plus a byte-identical regeneration test):
    - `evals/datasets/decisions/{dev,test}.jsonl`, 60/90: 15 templates x (4 dev, 6 test), labels by
      construction from `rules/rules.v2026.10.2.yaml` and the combination table (§4.4): clean low score →
      APPROVE; elevated → REVIEW `MODEL_SCORE_ELEVATED`; high → DECLINE `MODEL_SCORE_HIGH`; exact band edges
      0.30/0.85/0.2999; R001, R001+high, R002 (+elevated), R003, R004, R005, R009 (+model code); near misses
      (minutes=60, ratio 7.99, tx_count_10m 7 or amount 301, distinct 2 or amount 49,999, tx_count_1h 19) →
      APPROVE; model unavailable (floor 20,000; with rules); features unavailable (floor 5,000, R009 still
      fires on the fallback vector: `internal/features/fallback.go` keeps amount/channel/hour, ratio 1,
      new_device 0, distinct 2, so no other rule can fire). Background features must stay below every rule.
      Row (upstream Case shape, D-030) = {id, split, tags: [template], input: {transaction, features (13 names,
      consistent with the transaction) | null, model: {score} | {unavailable}}, expected: {action,
      reason_codes (sorted), degraded}}.
    - Target for the decisions suite: a small Go command (e.g. `cmd/evaldecide`) that runs the real
      `api.ScorerServer` with a stub feature store (case features or error), stub model (case score or
      error), the real rules engine and in-memory decisions; JSONL in, JSONL out; graders in `evals/graders/`
      (`test_reason_code_set_grader`, T-38).
    - `evals/datasets/summaries/{dev,test}.jsonl`, 48/72: CaseContext-shaped cases (decision, features,
      events; see `src/fraud_summarizer/pipeline.py::build_facts`) with the expected pattern by construction
      from `cmd/gen/PATTERNS.md`: ATO 12/18, card testing 8/12, mule 8/12, friendly fraud 8/12, unclear
      (legit look-alikes) 12/18; include low-and-slow variants that FakeLLM's heuristics miss (honest
      accuracy), MODEL_UNAVAILABLE cases, planted synthetic PII in merchant names (example.com e-mails,
      555 numbers, 4111 test cards) listed per case for the "PII to provider = 0" metric, and a few injection
      strings. Tune FakeLLM on dev only if pattern accuracy < 0.85.
    - `evals/datasets/judge_calibration.v1.jsonl`, 80 (40 useful / 40 not): good summaries written from the
      template facts vs corrupted ones (wrong pattern, wrong numbers, absent fact keys, generic checks).
    - The PreToolUse guard blocks edits of `evals/datasets/**/test*.jsonl` after generation: get the
      generator right before the first commit.
- E2E needs the whole stack: `FULL=1 scripts/seed_host.sh` (adds dev-issuer 18515, summarizer on the fake
  LLM 18513, webserve 18500), then `E2E_BASE_URL=http://127.0.0.1:18500 make e2e`; `make docs` style
  screenshots with `E2E_SCREENSHOTS=1` (see `tests/e2e/playwright.config.ts`). `docs/images/live.png` shows
  the idle stack after seeding; recapture it under load once a load generator exists (T-40/T-46).
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
| T-31 | Seed job and replay | M3 | done | internal/replay (warm via Kafka + wait; live per-account apply-then-score, D-025), cmd/replay, cmd/seed, internal/model/batcher.go; TestReplayRateLimit, TestSeedIdempotent, TestReplayDoesNotLeakLaterEventsIntoFeatures pass. On gen v2 data + spec v2 model (D-026): `scripts/seed_host.sh` (host stack) → seed exit 0 in 74 s, 16,904 scored, 0 errors; `scripts/seed_report.py` on that database → 74 open cases (≥ 50; REVIEW: 31 account takeover, 26 legitimate look-alikes, 14 mule, 3 other), 59 degraded, Score p50 3.5 / p95 8.6 / p99 12.7 ms. `make seed` itself runs the compose `seed` service, which lands with T-43. v1 numbers (144/148 cases) came from a leaking replay and are withdrawn |
| T-32 | Console scaffold and data layer | M4 | done | web/ (Vite 7 + React 19 + TanStack Query 5 + Tailwind 4; generated web/src/api/schema.d.ts; api/client.ts with 401 → clear session + /sign-in?next=; auth/session.ts; tokens.css with text-safe status tokens), cmd/webserve (SPA + CSP + /api,/auth,/summarizer proxies, unbuffered streaming); client.test.ts "401 clears the session and redirects to sign-in" passes; `pnpm -C web test` 32 passed incl. WCAG contrast checks; `pnpm -C web build` ok; go test -race ./cmd/webserve ok |
| T-33 | Queue and case detail | M4 | done | web/src/routes/{queue,case}.tsx, components/{AiSummary,LabelDialog,ReasonChips}.tsx; LabelDialog.test.tsx "previews the exact row to be written and sends the same values" passes; queue/case tests cover loading, empty, error-with-retry and success, keyboard navigation (j/k/Enter), evidence → fact highlight, role-gated Label/Regenerate; `pnpm -C web test` 41 passed (3 consecutive runs), `pnpm -C web build` ok. Summarizer regenerate now typed (gen/openapi/summarizer.json → web/src/api/summarizer.d.ts), idempotency key scoped per case, 20/hour per user |
| T-34 | Timeline, shadow, live metrics, rules | M4 | done | web/src/routes/{timeline,shadow,live,rules}.tsx, api/live.ts (NDJSON stream via openapi-fetch parseAs=stream, exponential reconnect 1→10 s), components/{Sparkline,StatTile}.tsx (series colour #0891b2 passes the dataviz validator in both themes); live.test.tsx "stream reconnects after error" passes; screens.test.tsx covers Test rule gating/compile errors, shadow role gate and band matrix, timeline links/empty; `pnpm -C web test` 50 passed (2 runs). Timeline decisions now carry case_id (TestListGetTimelineShadowAndRules) |
| T-35 | E2E, axe, screenshots | M4 | done | `tests/e2e/` (`label-case.spec.ts`: label by mouse and keyboard-only, focus trap, RBAC 403 for ml_engineer, sign-in redirect; `a11y.spec.ts`: axe WCAG 2.0–2.2 A/AA on sign-in, queue, case, timeline, shadow, live, rules and the open label dialog, both themes, 1280 and 375 px; `layout.spec.ts`: no horizontal overflow). On a fresh `FULL=1 scripts/seed_host.sh` stack (73 open cases): `E2E_BASE_URL=http://127.0.0.1:18500 make e2e` → 35 passed, 0 serious/critical axe violations; screenshots (10) in `docs/images/` via `E2E_SCREENSHOTS=1`. Found and fixed on the way: summarizer could not decode snappy batches and died silently, provider TypeError stalled the consumer (1b5feb0); shadow screen showed completions as drops (679cf64); focus trap escaped when the second radio was checked; rules table region not keyboard-reachable; long fact values overflowed the case panel and timeline at 375 px. |
| T-36 | Eval datasets | M5 | done | `uv run python scripts/gen_eval_sets.py` (seed 7) → `evals/datasets/{decisions,summaries,judge_calibration}/{dev,test,cases.v1}.jsonl` + `MANIFEST.json`; `uv run pytest tests/unit/test_eval_sets.py` (test_splits_disjoint_and_counts x3, manifest, byte-identical regeneration, consistency) → 8 passed. Labels cross-checked before commit: all 150 decision cases through `go run ./cmd/evaldecide` (real ScorerServer, rules 2026.10.2) → 0 mismatches; all 120 summary cases through the offline pipeline → 120 `ok`. Baseline found (dev only): FakeLLM pattern accuracy 30/48 = 0.625, planted PII reaches the provider in 5 dev cases (redactor gaps) — both for T-38/T-42. |
| T-37 | Model eval report | M5 | done | `training/evaluate.py`, `evals/suites/model.yaml`; `uv run pytest tests/unit/test_evaluate.py` (test_recall_at_fpr_on_known_curve, rule flags spend the FPR budget, stable shards, report shape, stale data refused) → 5 passed. `make gen-data SIZE=eval` (gen v2) → 2,000,000 events, 1,347,743 transactions, fraud 0.0079, sha256 632291c0507e (36.8 s). `make eval-model` (90 s; serving ONNX `fraud-gbdt/3` via `ModelRuntime`, rule flags from `cmd/evaldecide`) on the test split (169,604 tx, 1,680 fraud) → recall@1%FPR combined 0.9232 (≥ 0.80), PR-AUC 0.8740 (≥ 0.70), ECE 0.0007 (≤ 0.02); reported: model-only 0.9244, rules-only recall 0.6012 at FPR 0.0021; threshold chosen on valid → test recall 0.9161 at FPR 0.0079; `evalgate check evals/reports/model.json` → PASS. **Corrected after the M5 ML review (D-037):** the 0.9232 above fit its threshold on test. `make eval-model` now gates on thresholds never fit on test → combined at the valid-chosen threshold 0.9161 (FPR 0.0079 ≤ 0.015), at the active rule set's review threshold 0.30 recall 0.8387 at FPR 0.0032 (regression-gated), decline band (score ≥ 0.85) 0.5893, band calibration gap 0.0081 (≤ 0.05), PR-AUC 0.8740, ECE 0.0007; test-oracle 0.9232 reported only. `uv run pytest tests/unit/test_evaluate.py` → 9 passed. |
| T-38 | Decision and summary suites | M5 | done | `evals/suites/{decisions,summaries}.yaml`, `evals/graders/{decisions,summaries}.py` (targets: `cmd/evaldecide` over the real scorer; the real summary pipeline with every provider request recorded). `uv run pytest tests/unit/test_graders.py` (test_reason_code_set_grader, degraded handling, PII and schema graders, target runs the scorer) → 5 passed. Test split, offline: `make eval-decisions` → action_exact 1.0, reason_code_set 1.0, degraded_handling 1.0 (90 cases); `make eval-summary` → schema_valid 1.0, faithfulness 1.0, pattern_accuracy 0.9028 (CI 0.833–0.958), pii_to_provider 0.0, p95 4.9 ms (budget 200). Tuned on dev only: FakeLLM heuristics from PATTERNS.md (`tests/unit/test_fake_llm.py`, dev 0.625 → 0.9375) and the redactor (dot-separated cards, bare 10–11 digit phones; dev PII 0.104 → 0). Baselines and `evalgate check` gating arrive with T-40; `evalgate calibrate` (judge set) is still missing. |
| T-39 | Latency bench | M5 | done | `cmd/benchreport` (ghz JSON → `evalgate.report.v1`; one-second windows as paired cases; fails over the p95/p99 budget, under 99.9% success, under 95% of the target rate, or over 5% degraded decisions), `scripts/bench.sh` (host stack: compose.test.yaml, fresh DB, BentoML on `artifacts/model`, scorer; 10 s warm-up; `scripts/bench/score.json`: unique ids, 5,000 accounts, 9,000 devices). `go test ./cmd/benchreport` (TestBenchreportFailsOverBudget, windows, errors and missed rate, degraded rate, check-only) → ok. `make bench` (ci, 60 s, 4 vCPU container) → 300 rps achieved, p50 12.7 ms, **p95 21.8 ms** (< 80), p99 28.4 ms, success 1.0, degraded 0.0087. `make bench BENCH_PROFILE=laptop` → **fails here**: 586 rps achieved of 2,000, p95 603.8 ms, p99 728.5 ms, 99.3% degraded (load generator and every service on 4 shared vCPUs; KNOWN_LIMITATIONS). Re-run on a quiet host at f9ab2a5 (clean tree, load 0.5): 300 rps, p95 19.6 ms, degraded 0.0094. The M5 review measured degraded 0.0628 at load 8.5 (a concurrent build), over the 5% gate: see KNOWN_LIMITATIONS. |
| T-40 | REPORT.md and gate | M5 | done | `scripts/build_report.py` → `evals/REPORT.md` (mode, prompt hash, models, every metric vs threshold and baseline, `evalgate check` verdict per suite, the command behind each number, live evals stated as not run); `uv run pytest tests/unit/test_build_report.py` (test_report_states_mode, live/cassette mode, gate failures listed, live evals not run) → 4 passed. Baselines `evals/reports/baseline/{model,decisions,summaries,latency}.json` from commit beafcea (D-035). `make eval` → model, decisions, summaries `evalgate check` PASS against the baselines, latency within budget (p95 21.8 ms), REPORT.md Gate PASS (4 of 4). Then: judge `prompts/judge_summary_usefulness.v1.md` + `src/fraud_summarizer/judge.py` + rule-based FakeLLM judge; `make eval-judge` (offline, test split) → agreement 0.900, kappa 0.800 (min 0.8; dev was 0.950, no tuning); `scripts/eval_live.py` and `scripts/record_fixtures.py` with a USD budget (`evals/live.py`), tested offline with a stub provider (`tests/unit/test_eval_live.py`, 4 passed); `make eval-live` here → "not run: set ANTHROPIC_API_KEY" (exit 1). `make eval` → Gate PASS, 5 of 5 checks. |
| T-41 | Telemetry and dashboard | M6 | done | Go tracing: `internal/telemetry/tracing.go` (OTLP/gRPC export only when `OTEL_EXPORTER_OTLP_ENDPOINT` is set, W3C propagator, `trace_id`/`span_id` on JSON logs); otelgrpc server handler (health checks filtered) and gateway forwarding of `traceparent`; `fraud.score` span with `app.decision.*` attributes and children `features.score_tx`, `model.predict`, `rules.evaluate`, `postgres.commit`; traceparent persisted (`migrations/00005`, spool) and sent by the outbox relay as the Kafka `traceparent` header. Tests: `TestScoreSpanHasDecisionAttributes`, `TestDegradedScoreMarksTheSpan`, `TestScoreContinuesTheCallersTrace`, `TestLogsCarryTheTraceID`, `TestTracingIsANoOpWithoutAnEndpoint`, `TestTraceparentRoundTrip`, `TestRelaySendsTheScoreTraceparent`, `TestSpoolKeepsTheTraceparent` → pass (`go test -race`, store with `-tags integration`). Python: `fraud_summarizer/telemetry.py` (OTLP/HTTP only when configured, context from the Kafka `traceparent` header, JSON logs with `trace_id`), consumer span `summarizer.decision`, one `summarizer.<node>` span per step record with `app.step.*` and `gen_ai.*`, `fraud_llm_cost_usd_total{route,model}` and `fraud_llm_calls_total`; `uv run pytest tests/unit/test_summarizer_tracing.py` (test_step_record_otel_mapping, context from headers, cost counter, JSON logs) → 4 passed. Profile: `compose.yaml` `observability` (collector 8580/8581, Grafana 8590, Prometheus 8591, Jaeger 8592) and `datadog`; `deploy/otel`, `deploy/prometheus/{prometheus,alerts,alerts_test}.yml` (ScoreLatencyHigh, DegradedDecisions, OutboxUnsentAge), `deploy/grafana` (provisioning + `fraud.json` from `scripts/gen_dashboard.py`, palette validated); `make obs-check` → promtool config SUCCESS, alert unit tests SUCCESS, both collector configs valid; `tests/unit/test_observability.py` (registered metrics only, SLO thresholds, datasources, ports, generated dashboard) → 5 passed; `docs/runbook.md`. Live, rerun 2026-10-18 after the security fixes (Grafana 12.4.12, metrics on the admin listeners, W3C trace context without baggage; D-049, D-052, D-054): `scripts/obs_host.sh` (profile images on host networking + `FULL=1 seed_host.sh`, 16,904 decisions, 80 open cases) → `scripts/obs_verify.py` PASS: every dashboard query returns data except the event-only rule-error series (no events), Grafana serves the 13-panel dashboard, 29 of 29 summarized decision traces run from `fraud.v1.Scorer/Score` through `summarizer.persist` (14 spans). The verifier now samples summarized decisions only: on the shared test broker the summarizer also drains earlier runs' decisions, whose traces were never in this Jaeger. `docs/images/grafana-fraud.png` is from the first run, on Grafana 11.5.2. The compose-profile run after `make demo` waits on T-43/T-46. |
| T-42 | Security controls and threat model | M6 | done (1 open: govulncheck) | `docs/security.md` rewritten against the code (trust boundaries, STRIDE per component, OWASP LLM Top 10 2025, scan results, accepted and open findings). The M6 security audit's findings, each with a failing-first test: H1 `TestCompileExprRejectsUnboundedWork`; H2 `TestEventTimeIsBounded`; H3 redactor rewrite (`test_no_digit_of_adjacent_pii_survives`, 3,000 seeded adjacent pairs; `test_no_pii_reaches_provider`, 11 inputs × every route); M1 `TestServiceRolesAreLeastPrivilege`; M2 `test_free_text_evidence_must_quote_the_fact`, `test_recommended_checks_are_bounded`; M3 `TestReplayWithDifferentContentsIsRejected`; M4 `TestDevModeAndJWKSURL`, `TestNoTokenProxyOutsideDev`, `test_jwks_url_needs_https_outside_dev`; M5 `TestGRPCMetadataOverHeaderLimitRejected`, `TestConcurrentStreamsPerUserAreCapped`; M6 the model-server digest tests in `tests/unit/test_model_server.py`; M7 `test_paid_attempts_per_decision_are_capped`, `test_a_message_that_keeps_failing_goes_to_the_dlq`, `test_a_spent_daily_budget_stops_paid_calls_until_the_next_utc_day`; M8 `test_compose_publishes_on_loopback_only`, `test_grafana_has_no_default_credentials_and_prometheus_no_lifecycle_api`; M9 `TestAuditChainIsKeyed`, `TestAuditAnchorsDetectTruncation`; L1–L11 in D-049 to D-055 (`TestAllowlistsMatchTheTransactionSchema`, `TestReadyzDetailOnlyOnAdmin`, `TestGRPCHealthNeedsNoToken`, `TestRemoteUnsampledParentIsStillSampled`, `TestRemoteKeysSingleFlightBackoffAndStaleCap`, `test_console_requests_are_limited_per_user`, `test_service_containers_are_hardened`). Redactor parity: `uv run python scripts/redact_parity.py --rev ca63a13^` → 9 files, 42,990 string values (6,488 unique), 30 unique values differ, all UUIDs the old redactor corrupted. `make security` (2026-10-18): gitleaks, gosec (22 justified `#nosec`), bandit, pip-audit (205 packages), pnpm audit, hadolint, checkov, trivy on both images (vulnerabilities and secrets), licences (292 permissive, 7 exceptions, D-039) → pass; govulncheck NOT RUN (network policy), so the gate fails closed. `make test` → exit 0 (Go core coverage 86.2%, total 75.8%; Python 92.08%; 67 Vitest tests); `make eval` → all gates PASS. Retention (D-040, extended by 00009 and 00011): `TestRetentionDeletesOnlyExpiredRows` → ok. |
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
- [x] Every screen in §13 is implemented with loading, empty, error and success states; axe reports no serious violations; Playwright captured the screenshots in `docs/images/`. (Vitest state tests per screen; `make e2e` 35 passed with 0 serious axe violations; 10 screenshots, T-35. Recapture the screenshots after the M4 UX fixes: `timeline.png` shows the 100x amount bug.)
- [ ] The harness in §7 is complete: CLAUDE.md, AGENTS.md, settings, hooks with passing tests, rules, subagents, skills and `.mcp.json`.
- [ ] Every document in §18 exists, and every number in the README points to the command or report that produced it.
- [ ] The `reviewer` subagent's final verdict is "ship", and `security-review` findings are fixed or documented.
- [ ] `PROGRESS.md` has every task checked with evidence, and `docs/KNOWN_LIMITATIONS.md` lists what is not done.
- [ ] Every commit is authored and committed by the maintainer, and no commit message or pull request contains `Co-Authored-By`, `Claude-Session` or "Generated with Claude Code" (checked as in §19).
- [ ] `BUILD.md` lives at `docs/specs/000-build-spec.md`; all work is committed, and pushed when a remote exists.
- [ ] FR-1 to FR-13 each have a passing named test in `PROGRESS.md`.
- [x] `make bench` p95 < 80 ms at 300 RPS; the laptop run (2,000 RPS) is reported as measured. (ci: p95 21.8 ms at 300 rps; laptop profile measured and failing in the 4-vCPU container: 586 rps, p95 603.8 ms, 99.3% degraded — T-39, KNOWN_LIMITATIONS.)
- [x] §11 thresholds met: recall@1%FPR ≥ 0.80 (rules-only baseline reported), faithfulness ≥ 0.95, PII to provider 0. (`make eval-model`: combined at the valid-chosen threshold 0.9161, at the shipped review threshold 0.8387, rules-only 0.6012; `make eval-summary` offline: faithfulness 1.0, PII 0.0 — T-37, T-38.)
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
| FR-13 | `internal/api/cases_test.go::TestAnalystCannotTestRule`, `internal/store::TestLabelWritesAuditRow`, `tests/contract/test_openapi_contract.py`, `tests/e2e/label-case.spec.ts`, `tests/e2e/a11y.spec.ts` | pass (API, console unit tests, e2e 35 passed on the host stack) |

## Milestone reviews

| Milestone | Reviewer verdict | Notes |
|---|---|---|
| M0 | fix first → fixed | Blockers: `go vet ./...` parsed `Dockerfile.go` (fixed with `GO_PKGS`, regression test `tests/harness/test_go_packages.py`); settings deny `Read(./.env.*)` also blocked `.env.example` (replaced with explicit patterns). Non-blocking fixed: skeleton rejects bad `amount_minor`; CLAUDE.md lists every target. |
| M1 | fix first → fixed | Blockers fixed: consumer `Run` could hang on shutdown while a handler kept failing (select on send; regression `TestRunReturnsWhileHandlerKeepsFailing` fails without the fix); updater Redis client timed out pipelines (dedicated `NewUpdaterRedisClient`, 500-command chunks, benchmark rerun with it: 57,359 events/s); credential reads made exact (16-entry history per HASH field, exact `minutes_since` assertion) and `docs/KNOWN_LIMITATIONS.md` added. Non-blocking fixed: DLQ no longer duplicated on retries (`TestRetriedBatchDoesNotDuplicateDLQ`), insert-failure metric, `audit_log.prev_hash` UNIQUE, per-account timestamp assertion in offline features. Deferred: dev roles keep LOGIN with dev passwords (documented in `docs/security.md`, T-42). |
| M2 | fix first → fixed | Blockers fixed: `provider_schema` dropped the `pattern` property from provider schemas (now strips only constraint keywords; `test_provider_schema_keeps_every_property`); faithfulness regex missed numbers with units such as `12x`, `30min`, `.5` (`test_numbers_with_units_are_checked`); gateway retried invalid JSON/schema output against Appendix B (now non-retryable, moves to the fallback provider — D-014). Non-blocking fixed: BentoML request-buffer pool race removed, default 20 ms deadline in model clients, expr builtins restricted + rule runtime-error metric + file/YAML version check + OnLoad lock (D-016), cache-aware cost (D-015), facts fence escaping, cassette `recorded` defaults to false, SSN and date/time redaction false positives, both-down floor recorded (D-012). ML review: split, calibration, ONNX parity and D-008 judged sound. Deferred items listed under Session handoff. |
| M3 | fix first → fix first → ship | Round 1 blockers: replay leaked later device sightings into features (fixed with per-account apply-then-score, `TestReplayDoesNotLeakLaterEventsIntoFeatures`; the leak-inflated 144/148 open-case evidence was withdrawn and T-31 rerun on gen v2, D-026); a hung Postgres overloaded the writer (breaker + spool, D-027, `TestHungPostgresSpoolsInsteadOfOverloading`). Round 2 blockers: the breaker never closed under continuous traffic (closes on a successful ping, `TestBreakerClosesUnderContinuousTraffic`); the leak test flaked on a cold Redis (warm-up, degraded decisions fail the test). Round 3 (2cec37e): ship — reviewer reran its breaker probe (closes in 21–530 ms), `-count=30 -run Leak` under load 30/30, full `-race -tags integration` suite exit 0, `seed_report.py` reproduced the T-31 numbers. Non-blocking (open): leak test still bound to the 8 ms Redis budget on an overloaded host; half-hung Postgres reopens the breaker once per replay interval; Commit/Close race can reopen a spool file; BentoML expected feature spec is an env default, not read from `features/spec.yaml`. |
| M4 | fix first (code) + fix first (UX) → ship (code) + fix first (UX) → ship | Round 1 on 2ef1a63: code reviewer 1 blocker (a 429 on regenerate consumed its Idempotency-Key, fixed in 3d8c331, `test_a_rate_limited_regenerate_does_not_consume_its_key`); UX reviewer 7 blockers (queue cursor, label-dialog focus, evidence scroll, timeline amounts 100x, field-border contrast, skip link, colliding UUIDv7 short IDs), fixed in 3b48a6e with failing-first Vitest tests, `TestListGetTimelineShadowAndRules` (server title) and Tab-only e2e. Round 2: code reviewer ship (7790ca3); UX reviewer 1 regression (row-index cursor drifted on refetch), fixed in 60cf07d (`keeps the cursor on the same case when a refetch reorders the list`) → ship. Evidence: `cd web && npx vitest run` → 67 passed; `make check-fast` → exit 0; `FULL=1 scripts/seed_host.sh` + `E2E_BASE_URL=http://127.0.0.1:18500 make e2e` → 49 passed, with `E2E_SCREENSHOTS=1` → 51 passed (axe 1280/768/375 px, both themes, 0 serious); `go test -race ./cmd/webserve` → ok. |
| M6 | fix first (code) + security audit (3 high, 9 medium, 11 low) + latency review → fix first + fix first (round 2) → fix first + fix first (round 3) → fix first + fix first (round 4) → fix first + ship (round 5) → fixed, round 6 pending | Round 1 (T-41, T-42): code reviewer 2 blockers (adjacent PII leaked past the redactor; `security.sh` passed when a scanner could not run) and non-blocking items (outbox foreign-key index, alert tests far from the thresholds, Score span error and duplicate attributes, a health-filter test that did not exercise it, an uncited count); security-auditor H1–H3, M1–M9, L1–L11 and 13 claims in `docs/security.md` the code did not back; latency reviewer: tracing within budget, 3 allocation fixes. All fixed in D-041 to D-056 (see T-42), Go moved to 1.26.9 (D-042, maintainer's choice). Not taken: otelgrpc only when tracing is on, dropped-span metric (D-056). Round 2 on 1d1b596: code reviewer 2 blockers (a table owner could rewrite the audit chain as unkeyed rows, and `audit-verify` without anchors printed intact; an unprefixed Redis value was served), security-auditor 1 High (cards glued to hex or separated by commas or colons reached the provider) and fail-open Mediums (model with no recorded digest, faithfulness without the fact's value, unpriced models and timeouts free against the budget, anonymous health Watch, development DSNs, flat transient backoff). Fixed in 16bdf75, cace120, 8990794 and 05bdd3d (D-057 to D-060); `make test` exit 0 (Go core 86.5%, Python 92.34%), `make eval` PASS, `make security` all pass except govulncheck (network). Round 3 on 0345f58: code reviewer 3 blockers (unkeyed audit rows at seq <= 0 slipped under the legacy bound; short fact values matched as substrings; separated cards and phones touching letters leaked), security-auditor 4 Mediums (a stored decision rewritten in Redis under its hash was served; two card formats; billed max_tokens and refusal responses free against the budget; faithfulness residue) and Lows (DSN check on the URL text, self-attesting model metadata). Fixed in 8b1bf2e and the two commits after it (D-061 to D-063); `make test` exit 0 (Go core 86.7%, Python 92.49%), `make eval` PASS, `make security` all pass except govulncheck (network). Round 4 on acf152c: code reviewer 7 blockers and security-auditor 2 Mediums, 4 Lows (D-064); fixed in the commit after it. `make test` exit 0 (Go core 86.6%, Python 92.67%, 483 pytest, 67 Vitest), `make eval` PASS (offline summary p50 6.3 → 12.0 ms from the redactor's three candidate passes, `evals/REPORT.md`), `make check-fast` and the lint targets 0, `redact_parity.py --rev acf152c` 0 of 6,488 unique values differ. `make security` on rebuilt images: all pass (trivy on both images, vulnerabilities and secrets) except govulncheck NOT RUN (network policy). Round 5 on e92eaa1: code reviewer 1 blocker (`%40` no longer decoded let `pay%404111...` through whole), security-auditor ship with Lows (tie-breaking left most of a real card, encoded and non-standard layouts, hyphenated clearing phrases, service-file and multi-host passfile parity) (D-065); fixed in the commit after it. |
| M5 | fix first (code) + fix first (ML) → ship + ship | Round 1 on ed94e60: code reviewer 2 blockers (judge calibration JSON inside the report.v1 glob left `make check-fast` red after `make eval`; a missing baseline or suite still passed the gate), fixed in 6bb26f9 (`test_calibration_output_is_outside_the_report_glob`, `test_missing_required_suite_fails_the_gate`, `test_missing_baseline_fails_the_gate`); ML reviewer 2 blockers (gated combined recall fit its threshold on test and the shipped review threshold was never measured; ECE cannot fail at a 1% base rate), fixed in 02aa3b3 (D-037: valid-chosen threshold, review-band recall 0.8387 at FPR 0.32%, band calibration gap; `test_gated_combined_recall_uses_the_valid_threshold`, `test_band_calibration_gap_catches_what_ece_misses`). Round 2 on fc88be5: both ship, numbers reproduced from clean `git archive` copies. Non-blocking taken in 45d9232: pooled shard calibration gap, untracked files count as dirty, shipped-threshold line in REPORT, review-only numbers removed from KNOWN_LIMITATIONS. Open: offline judge reuses FakeLLM's classifier; PII formats not held out; `evalgate check` does not pin the data hash; `select()` guards only the first byte of an evaldecide reply. Evidence: `make eval` on a clean tree → Gate PASS (5 of 5); `make check-fast` → exit 0; `make bench` (quiet host, f9ab2a5) → p95 19.6 ms. |
