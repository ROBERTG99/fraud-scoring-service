# Security

Threat model and controls for the fraud scoring service (spec §15, T-42). All data is synthetic; the system
is single-tenant and runs locally or in one VPC. `make security` runs every scanner (`scripts/security.sh`,
which fails closed when a scanner cannot run); the latest results, the accepted findings and what is still
open are at the end of this page. The M6 security audit's findings (two rounds) and where each was fixed are
listed in `docs/DECISIONS.md` (D-041 to D-060).

`FRAUD_ENV=dev` marks a local stack (`compose.yaml`, `scripts/seed_host.sh`, the tests). Anything else is
treated as a deployment, and the development shortcuts below refuse to run there: the dev-issuer, plain-HTTP
JWKS, the development audit key, the migrations' development database passwords and a lifted `event_ts`
bound (D-045, D-050, D-059, D-060).

## Trust boundaries

| Boundary | Who is on the far side | Authentication | Limits |
|---|---|---|---|
| `Score`: gRPC 8510, HTTP 8511 `POST /v1/score` | the card platform (a service) | static service token, compared in constant time (`internal/api/interceptors.go`) | 64 KB message or body; 16 KB of headers or gRPC metadata, enforced by the server (`TestBodyOver64KBRejected`, `TestServerRejectsOversizedMetadataBeforeAuth`); 1,024 streams per connection, 5 s handshake, client pings at most every 10 s (D-046); field validation with currency and MCC allowlists (`internal/api/validate.go`, D-051); `event_ts` within [now − 24 h, now + 5 min] (D-041); a replayed `transaction_id` must carry the same request (SHA-256, D-044); 45 ms deadline |
| Console APIs: `CaseService` (gRPC, gateway) and the summarizer (HTTP 8513) | analysts, risk and ML engineers | issuer JWT, EdDSA only; issuer, audience, expiry and a known role checked in Go and Python (`internal/auth/jwt.go`, `src/fraud_summarizer/auth.py`); JWKS over https outside dev mode (D-045), cached for at most an hour past the issuer's last answer (D-053) | RBAC per method, deny by default (`internal/auth/rbac.go`); 20 req/s per user on both services (`internal/api/ratelimit.go`, `TokenBucket` in the summarizer); 4 open live-metric streams per user (D-046); regenerate 20 per hour per user, counted in Postgres (D-054) |
| LLM provider: routes `case_summary`, `judge_usefulness` (live evals only) | a third-party API | provider key from the environment | redacted, fenced input; schema-validated output; `max_tokens` 2,048 and 1,024; timeouts 8 s and 15 s; 3 attempts per provider (`config/llm_routes.yaml`); 3 paid attempts per decision and a daily budget (D-048) |
| Postgres, Redis, Kafka, model server | internal services | Postgres: one role per service (D-043), and outside dev mode the services refuse the migrations' development passwords (D-059); Redis: clients send `FRAUD_REDIS_USERNAME` / `FRAUD_REDIS_PASSWORD` when set, but no ACL is defined in this repository yet (open finding); Kafka and the model server: network isolation only | compose publishes every port on 127.0.0.1 (D-049); service containers are read-only, without capabilities (D-055) |
| Operators | whoever runs the stack | none on the admin listeners (scorer 8516, summarizer 8517; loopback by default, never published) | `/metrics` and the detailed `/readyz` live there only (D-052, D-054) |

Only `src/fraud_summarizer/llm/` imports a provider SDK, and nothing on the scoring path makes a network call
other than the model server, Redis and Postgres. The summarizer reads the decision it summarizes; its database
role cannot write `decisions`, labels or cases (`TestServiceRolesAreLeastPrivilege`).

The dev-issuer mints a token for any persona that asks. It starts only with `FRAUD_ENV=dev`, listens on
127.0.0.1 by default, and `webserve` proxies to it only in dev mode (`TestNoTokenProxyOutsideDev`); `webserve`
and the feature-updater also listen on 127.0.0.1 by default (D-060). On the
local stack the personas share one subject per role, so per-user limits are per persona there.

## STRIDE per component

| Component | Threat | Control |
|---|---|---|
| Scorer | **S**poofed caller | Service token (constant-time compare); console tokens cannot call `Score`, the service token cannot call console methods. Only the gRPC health `Check` needs no token and reveals only SERVING; `Watch` and `List` need one (`TestGRPCHealthNeedsNoToken`). |
| | **T**ampered request | Field validation: UUIDs, `acc_` tokens, the 19 catalog MCCs, the ISO 4217 allowlist, /24 IP prefixes, salted device hashes, merchant names of at most 120 characters without C0 or C1 controls or bidi marks; JSON nulls and unknown query parameters rejected. `event_ts` outside its window is rejected before Redis is touched, so a caller cannot rewrite an account's history (`TestEventTimeIsBounded`); the past bound can be lifted only in dev mode, for replays. A replay with other contents answers ALREADY_EXISTS on every path (`TestReplayWithDifferentContentsIsRejected`), including a concurrent duplicate, whose stored hash comes back with the stored decision (`TestDuplicateIsCheckedWithTheHashCommitReturns`). |
| | **R**epudiation | Each decision is stored with its id, request hash, rule-set and model versions, reason codes and features. |
| | **I**nformation disclosure | Responses carry decision fields only; spans and INFO logs carry no account, device or merchant ids (`TestScoreSpanHasDecisionAttributes`); the public `/readyz` of every service says only "not ready" (`TestReadyzDetailOnlyOnAdmin`, `test_public_readyz_names_no_cause`). |
| | **D**enial of service | Body, header and metadata limits; stream, handshake and keepalive limits (D-046); 45 ms deadline with rules-only fallback; bounded writer queue (`ErrOverloaded`) and disk spool. |
| | **E**levation of privilege | `TestRule` (risk engineers) compiles caller expressions against an allowlist of AST nodes, at most 200 nodes, with `min`, `max` and `abs` as the only builtins, and stops on cancellation (`TestCompileExprRejectsUnboundedWork`). Rule changes are versioned YAML files loaded from disk. |
| Console APIs and `webserve` | Spoofing | EdDSA JWTs only (no `alg` negotiation), JWKS from the issuer over https outside dev mode, single-flight refresh outside the lock with backoff (`TestRemoteKeysSingleFlightBackoffAndStaleCap`). |
| | Elevation of privilege | RBAC table: `LabelCase` analyst, `TestRule` risk engineer, shadow summary risk or ML engineer; a method missing from the table is denied. |
| | Tampering | `LabelCase` writes the label and a keyed `audit_log` row in one transaction. |
| | Denial of service | Rate and stream limits above; 413 for bodies over 64 KB (`webserve` and gateway); the SPA server cannot leave `web/dist` (`TestSPAFallbackCachingAndHeaders`). |
| Postgres | Tampering with history | `audit_log` rows are HMAC-SHA256 over length-prefixed fields under `FRAUD_AUDIT_KEY`, held outside the database (D-050); a trigger rejects UPDATE, DELETE and TRUNCATE for every role that does not own the table. Unkeyed entries verify only up to `FRAUD_AUDIT_LEGACY_MAX_SEQ` (default 0), so the chain cannot be rewritten as unkeyed rows (`TestAuditRewriteAsUnkeyedIsRejected`, D-057). The owner (the migration role, which no service uses) or a superuser can still disable the trigger: `scorer audit-anchor` writes the chain head, chained to the previous anchor, outside the database, and `audit-verify` (which refuses to run without anchors outside dev mode) detects removed or rewritten entries up to the last anchor (`TestAuditAnchorsDetectTruncation`), provided the anchors file itself cannot be truncated. |
| | Elevation of privilege | `fraud_app` (scorer, relay, console) inserts decisions, labels, shadow rows, account events and audit rows, updates the outbox, cases and rule sets, and can neither update nor delete a decision or a label; `fraud_summarizer` writes only its own tables; only `fraud_retention` deletes (D-043). Retention (D-040): decisions and their rows 400 days, sent outbox 7 days, step records, processed messages and summarizer bookkeeping 90 days, audit log kept. |
| Kafka | Tampering, duplicates | Every message validated against its JSON Schema (the same allowlists as Score); invalid messages go to `events.dlq`; consumers deduplicate on ids. |
| Redis | Tampering with features | Writes only through fixed Lua scripts; idempotency entries carry the request hash and a value without it is never served, so a planted entry does not decide a real request (D-044, D-057, `TestRedisIdempotencyFirstWriteWins`). Feature keys themselves are protected only by network isolation until Redis ACL users exist (open finding). |
| Model server | Tampering with the model | The ONNX bytes must match every recorded SHA-256 (a pinned `FRAUD_MODEL_SHA256_<ALIAS>`, the registry tag, `model_meta.json`) before a session is built, and a model with none recorded is refused (D-047, D-059); a download is verified before it replaces the cache. |
| | Denial of service | 20 ms deadline from the scorer; a slow or failed model degrades to rules-only and is counted (`fraud_degraded_total`). |
| Case summarizer | See the LLM section below. | |
| Observability profile | Information disclosure | Loopback only; Grafana 12.4.12 with no credential at all (no admin user, no basic auth, no login form) and read-only anonymous viewers; Prometheus without its lifecycle API (D-049). A shared Grafana needs anonymous access off and an admin password from a secret. |

## OWASP Top 10 for LLM applications (2025)

| Risk | Exposure here | Control |
|---|---|---|
| LLM01 Prompt injection | Merchant names and account-event payloads are third-party text inside the prompt. | Facts are fenced as JSON data with `<`, `>` and backticks escaped (`test_fact_values_cannot_break_the_fence`); the prompt says to ignore instructions in data; output is schema-validated; each evidence item must cite a present fact and carry it (a numeric fact's number, a non-numeric fact's value or one of its items, a quote of at least 4 characters for free text such as `merchant_name`); statements and recommended checks with case-clearing language ("approve", "not fraud", "verified merchant") are dropped; `recommended_checks` are bounded to 200 characters each (`test_statements_must_carry_their_fact_and_must_not_clear_the_case`, `test_recommended_checks_are_bounded`, D-059). The screen is a word list: clearing language it does not know passes. `test_injection_in_merchant_name_is_ignored` runs offline against FakeLLM, so it tests the pipeline's checks, not a model's resistance; no live run has been made (no provider keys here). |
| LLM02 Sensitive information disclosure | Facts could carry PII planted in free text. | `redact.py` removes emails, phone numbers, card numbers (13–19 digits, Luhn; groups separated by whitespace, `-`, `.`, `/`, `_`, `,` or `:`, or glued to letters or hex) and SSNs after Unicode normalisation, including adjacent items, and repeats until nothing changes (D-058); it runs at the gateway for every route and on the facts before truncation (`test_no_pii_reaches_provider`, 15 inputs over every route, the case-facts path and the judge; a seeded fuzz of 3,000 adjacent pairs, `test_no_digit_of_adjacent_pii_survives`). Out of scope: names, postal addresses, IBANs, obfuscated addresses ("john(at)example"), masked card numbers and bare 9-digit SSNs without an "SSN" keyword. It errs toward removing digits: a device hash containing a Luhn-valid 13–19 digit run loses it (`docs/KNOWN_LIMITATIONS.md`). Devices are salted hashes and IPs /24 prefixes before they reach the scorer. Offline evals: `pii_to_provider` 0 (`evals/REPORT.md`). |
| LLM03 Supply chain | Provider SDKs, model artifacts, base images. | Locked dependencies (`uv.lock`, `pnpm-lock.yaml`, `go.sum`); the Dockerfiles' base images pinned by digest (D-055; the third-party images in `compose.yaml` and `compose.test.yaml` are pinned by version tag); `make security`; the scoring model is our own ONNX artifact, verified by SHA-256 at load (D-047). |
| LLM04 Data and model poisoning | Training data and labels. | Synthetic generator with a fixed seed; analyst labels are insert-only and audited; retraining is an explicit `make train` with a new registry version and shadow comparison before promotion. |
| LLM05 Improper output handling | Summary text rendered in the console. | Output must match the JSON schema; the console renders text, never HTML; uncited evidence is dropped; a failed or unfaithful summary falls back to a templated fact list. |
| LLM06 Excessive agency | The summarizer process. | A fixed pipeline with no tools; its database role writes only summaries and bookkeeping (D-043). Network egress beyond the provider is not restricted by any policy here: a deployment must enforce it. |
| LLM07 System prompt leakage | The prompt is in the repository. | The prompt holds no secrets; keys come only from the environment. |
| LLM08 Vector and embedding weaknesses | Not used. | No retrieval or embeddings. |
| LLM09 Misinformation | A summary could be wrong. | It never changes a decision; faithfulness checks drop uncited claims; `confidence` and the status (`ok`, `fallback`, `unfaithful`) are shown; the analyst decides. |
| LLM10 Unbounded consumption | Provider cost and latency. | Per call: `max_tokens`, 8 s (`case_summary`) or 15 s (`judge_usefulness`) timeouts. Per summary: 3 attempts with backoff on each of at most two providers, so up to 6 calls and about 2 × 3 × 8 s. Per decision: 3 summaries (pipeline runs), recorded before the call, then the DLQ, so at most 18 calls; a message failing 5 times for a non-transient reason goes to the DLQ too, and retries back off from 1 s to 30 s. Per day: `FRAUD_LLM_DAILY_BUDGET_USD` (default 25), after which summaries are templated; an unpriced route model is refused at start, an unpriced response model is charged the highest known price, and a timed-out call is charged an estimate (D-059); counted per process (`docs/KNOWN_LIMITATIONS.md`). Per user: regenerate 20 per hour. `fraud_llm_cost_usd_total` is on the dashboard; live evals stop at `LIVE_EVAL_BUDGET_USD`. |

## Scan results

`make security` on 2026-10-18 (raw reports in `artifacts/security/`):

| Scanner | Result | Notes |
|---|---|---|
| gitleaks | pass | full history |
| gosec | pass | 22 `#nosec` annotations in `cmd/` and `internal/`, each with its reason on the line; grouped below |
| govulncheck | **not run** | `vuln.go.dev` is blocked by the build environment's network policy; the gate fails closed |
| bandit | pass | `-ll` over `src`, `training`, `serving`, `scripts`, `evals` |
| pip-audit | pass | 205 locked packages, no known vulnerabilities |
| pnpm audit (high) | pass | one accepted advisory below |
| hadolint, checkov (Dockerfiles) | pass | |
| trivy, `fraud/case-summarizer:dev` | pass | vulnerabilities (HIGH, CRITICAL, fixed) and secrets |
| trivy, `fraud/scorer:dev` | pass | Go 1.26.9 with the upgraded modules (D-042) |
| trivy, model server and MLflow images | not scanned | `security.sh` scans the `fraud/` images of `compose.yaml`; these two images are not in it yet (T-43) |
| tflint, actionlint | n/a | no Terraform (T-44) or workflows (T-45) yet |
| licence check | pass | 292 permissive, 7 recorded exceptions (D-039) |

## Accepted findings

| Finding | Where | Why it is accepted |
|---|---|---|
| gosec G304 / G703 (file path from a variable) | `cmd/gen`, `cmd/scorer/audit.go`, `internal/features/spec.go`, `internal/rules/engine.go`, `internal/replay`, `internal/store/spool.go` | Paths come from the operator's configuration or command line (spec, rules directory, dataset, spool, audit anchors), never from a request. |
| gosec G703 | `cmd/webserve/main.go` | The path is `path.Clean("/"+p)` joined under `dist` and only `Stat`ed; tested with `../../etc/passwd`. |
| gosec G204 (subprocess with variable) | `cmd/seed/main.go`, `internal/testutil` | Operator-configured commands (the seed's `gen` and `train` steps) and fixed test binaries. |
| gosec G107 (HTTP URL from a variable) | `internal/telemetry/logging.go` | The container healthcheck probes its own configured local URL. |
| gosec G404, G115 | `cmd/gen/generator.go`, `internal/replay` | Synthetic data must be reproducible from the seed; conversions are masked or bounded. |
| gosec G118 (goroutine context) | `internal/stream/consumer.go` | Partition workers are owned by their quit and done channels; the rebalance callback's context is the client's. |
| GHSA-vfj7-8cjw-p6xm (`braces`, high) | `markdownlint-cli2` (dev only) | No patched release exists; the input is this repository's own glob patterns. Recorded in `web/package.json` `pnpm.auditConfig.ignoreGhsas`. |
| MPL-2.0, LGPL-3.0 (psycopg) | Python dependencies | D-039: used unmodified; LGPL only for the psycopg driver family. |
| CKV_DOCKER_7 | `Dockerfile.go` | The base image is a build argument whose default is pinned by digest. |

## Open findings

1. **govulncheck not run** here (network policy). It must run where `vuln.go.dev` is reachable (CI) before a
   release; until then trivy's module scan of the scorer image stands in for it.
2. **Redis has no ACL users yet.** Clients send `FRAUD_REDIS_USERNAME` / `FRAUD_REDIS_PASSWORD` when set, but
   no ACL is defined here and `compose.yaml` has no Redis until T-43 (D-044 describes the plan). Until then
   anyone who reaches Redis can change feature keys; idempotency entries are protected by their hash (D-057).
3. **Development credentials in the migrations.** The service roles are created with LOGIN and development
   passwords; services refuse those passwords outside dev mode (D-059), but a deployment must still set real
   passwords (or `NOLOGIN` and its own roles) when it migrates.
4. **No egress policy.** Nothing here stops a compromised summarizer dependency from calling out; a
   deployment needs a network policy that allows only the provider endpoints.
5. **Anchors need append-only storage.** The anchors file shows a removed middle line, but not removed newest
   lines; it must live where the database's operators cannot truncate it (`docs/runbook.md`).
6. **Images not scanned.** The model server and MLflow images are outside the trivy scan, and the third-party
   images in the compose files are pinned by tag, not digest.
7. **Trust that remains with a process.** The scorer holds the audit key and the summarizer's daily budget is
   per process; both are described in `docs/KNOWN_LIMITATIONS.md`.
