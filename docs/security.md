# Security

Threat model and controls for the fraud scoring service (spec §15, T-42). All data is synthetic; the system
is single-tenant and runs locally or in one VPC. `make security` runs every scanner (`scripts/security.sh`);
the latest results and every accepted or open finding are listed at the end of this page.

## Trust boundaries

| Boundary | Who is on the far side | Authentication | Limits |
|---|---|---|---|
| `Score` (gRPC 8510, HTTP 8511 `POST /v1/score`) | the card platform (a service) | static service token, compared in constant time (`internal/api/interceptors.go`) | 64 KB body, 16 KB headers (`internal/api/limits.go`), field validation and allowlists (`internal/api/validate.go`), 45 ms deadline |
| Console APIs (`CaseService`, summaries) | analysts, risk and ML engineers | dev-issuer JWT, EdDSA only, issuer, audience, expiry and a known role (`internal/auth/jwt.go`) | RBAC per method, deny by default (`internal/auth/rbac.go`); 20 req/s per user; regenerate 20/hour per user |
| LLM provider (`case_summary`, `judge_usefulness`) | a third-party API | provider key from the environment | redacted, fenced input; schema-validated output; `max_tokens`, 8 s timeout, list-price cost tracking |
| Kafka, Redis, Postgres, model server | internal services | network isolation (compose network / VPC); Postgres roles | containers are offline at runtime |

Only `src/fraud_summarizer/llm/` imports a provider SDK, and nothing on the scoring path makes a network call
other than the model server, Redis and Postgres. The LLM never reads or writes `decisions`.

## STRIDE per component

| Component | Threat | Control |
|---|---|---|
| Scorer | **S**poofed caller | Service token (constant-time compare); console tokens cannot call `Score`, the service token cannot call console methods. |
| | **T**ampered request | Field validation (UUIDs, `acc_` tokens, 4-digit MCC, ISO 4217 currency allowlist, /24 IP prefixes, salted device hashes, merchant name ≤ 120 characters without control characters); JSON nulls and unknown query parameters rejected. |
| | **R**epudiation | Each decision is stored with its id, rule-set and model versions, reason codes and features; idempotent replay returns the stored decision. |
| | **I**nformation disclosure | Responses carry decision fields only; spans and INFO logs carry no account, device or merchant ids (`TestScoreSpanHasDecisionAttributes`). |
| | **D**enial of service | 64 KB body (`TestBodyOver64KBRejected`), header and read timeouts, 45 ms deadline with rules-only fallback, bounded writer queue (`ErrOverloaded`) and disk spool. |
| | **E**levation of privilege | No admin surface; rule changes are new versioned YAML files loaded from disk. |
| Console APIs and `webserve` | Spoofing | EdDSA JWTs only (no `alg` negotiation), JWKS from the issuer, expiry and audience checked. |
| | Elevation of privilege | RBAC table: `LabelCase` analyst, `TestRule` risk engineer, shadow summary risk or ML engineer; a method missing from the table is denied. |
| | Tampering | `LabelCase` writes the label and an `audit_log` row in one transaction. |
| | Denial of service | 20 req/s per user; 413 for bodies over 64 KB (`webserve` and gateway); the SPA server cannot leave `web/dist` (`TestSPAFallbackCachingAndHeaders`). |
| Postgres | Tampering with history | `audit_log` is append-only (a trigger rejects UPDATE and DELETE; `fraud_app` has SELECT and INSERT only) and hash-chained (`make audit-verify`). |
| | Information disclosure | Least-privilege roles `fraud_app` and `fraud_readonly`; retention (`cmd/retention`, D-040): decisions and their rows 400 days, sent outbox 7 days, step records and processed messages 90 days, audit log kept (`TestRetentionDeletesOnlyExpiredRows`). |
| Kafka | Tampering, duplicates | Every message validated against its JSON Schema; invalid messages go to `events.dlq`; consumers deduplicate on ids. |
| Redis | Tampering with features | Only the feature-updater and scorer write, through fixed Lua scripts; keys carry a prefix per environment. |
| Model server | Denial of service | 20 ms deadline from the scorer; a slow or failed model degrades to rules-only and is counted (`fraud_degraded_total`). |
| Case summarizer | See the LLM section below. | |

## OWASP Top 10 for LLM applications (2025)

| Risk | Exposure here | Control |
|---|---|---|
| LLM01 Prompt injection | Merchant names and account-event payloads are third-party text inside the prompt. | Facts are fenced as JSON data with escaping (`test_fact_values_cannot_break_the_fence`); the prompt says to ignore instructions in data; output is schema-validated; every evidence item must cite a present fact with matching numbers or it is dropped (`test_injection_in_merchant_name_is_ignored`). The judge fences the candidate summary the same way. |
| LLM02 Sensitive information disclosure | Facts could carry PII planted in free text. | `redact.py` removes emails, phone numbers, Luhn-valid card numbers and SSNs before any provider call, at the gateway for every route and in the facts before truncation (`test_no_pii_reaches_provider`, 10 formats, every route, the case-facts path and the judge); devices are salted hashes and IPs /24 prefixes before they reach the scorer. Offline evals: `pii_to_provider` 0 (`evals/REPORT.md`). |
| LLM03 Supply chain | Provider SDKs and model artifacts. | Locked dependencies (`uv.lock`, `pnpm-lock.yaml`, `go.sum`); `make security` (pip-audit, pnpm audit, trivy, licence check); the scoring model is our own ONNX artifact with a recorded SHA-256. |
| LLM04 Data and model poisoning | Training data and labels. | Synthetic generator with a fixed seed; analyst labels are audited; retraining is an explicit `make train` with a new registry version and shadow comparison before promotion. |
| LLM05 Improper output handling | Summary text rendered in the console. | Output must match the JSON schema; the console renders text, never HTML; uncited evidence is dropped; a failed or unfaithful summary falls back to a templated fact list. |
| LLM06 Excessive agency | — | The summarizer is a fixed pipeline with no tools, no write path to decisions and no network access beyond the provider call. |
| LLM07 System prompt leakage | The prompt is in the repository. | The prompt holds no secrets; keys come only from the environment. |
| LLM08 Vector and embedding weaknesses | Not used. | No retrieval or embeddings. |
| LLM09 Misinformation | A summary could be wrong. | It never changes a decision; faithfulness checks drop uncited claims; `confidence` and the status (`ok`, `fallback`, `unfaithful`) are shown; the analyst decides. |
| LLM10 Unbounded consumption | Provider cost and latency. | `max_tokens` per route, 8 s timeout, bounded retries, regenerate limited to 20/hour per user, `fraud_llm_cost_usd_total` on the dashboard, `LIVE_EVAL_BUDGET_USD` for live evals. |

## Scan results

`make security` on 2026-10-17 (commit after T-42's fixes; raw reports in `artifacts/security/`):

| Scanner | Result | Notes |
|---|---|---|
| gitleaks | pass | full history |
| gosec | pass | 2 findings fixed in code (checked action mapping), 5 suppressed with a reason next to them (see below) |
| govulncheck | **not run** | `vuln.go.dev` is not reachable from the build environment's network policy |
| bandit | pass | 2 findings fixed (model cache no longer in a fixed `/tmp` path; URL scheme check) |
| pip-audit | pass | 205 locked packages, no known vulnerabilities |
| pnpm audit (high) | pass | `react-router-dom` 7.9.6 → 7.18.4 (7 high advisories, shipped code); `vitest` 3 → 4 (critical `tinypool` advisories); `markdownlint-cli2` 0.18 → 0.23 (`js-yaml`); one accepted advisory below |
| hadolint, checkov (Dockerfiles) | pass | |
| trivy, `fraud/case-summarizer:dev` | pass | vulnerabilities and secrets |
| trivy, `fraud/scorer:dev` | **fail** | 48 HIGH/CRITICAL in the Go standard library and modules; open finding below |
| tflint, actionlint | n/a | no Terraform (T-44) or workflows (T-45) yet |
| licence check | pass | 292 permissive, 7 recorded exceptions (D-039) |

## Accepted findings

| Finding | Where | Why it is accepted |
|---|---|---|
| gosec G404 (math/rand) | `cmd/gen/generator.go` | Synthetic data must be reproducible from the seed; nothing generated is a secret. |
| gosec G115 (integer conversion) | `cmd/gen/generator.go` | UUIDv7 random bits: masked or intentionally truncated. |
| gosec G703 (path traversal) | `cmd/webserve/main.go` | The path is `path.Clean("/"+p)` joined under `dist` and only `Stat`ed; tested with `../../etc/passwd`. |
| gosec G703 | `internal/testutil/modelserver.go` | Test helper writing into a test temp dir. |
| gosec G118 (goroutine context) | `internal/stream/consumer.go` | Partition workers are owned by their quit/done channels; the rebalance callback context is the client's. |
| GHSA-vfj7-8cjw-p6xm (`braces`, high) | `markdownlint-cli2` (dev only) | No patched release exists; the input is this repository's own glob patterns. Recorded in `web/package.json` `pnpm.auditConfig.ignoreGhsas`. |
| `smol-toml` (moderate), `katex` (low) | `markdownlint-cli2` (dev only) | Below the high threshold; dev tooling, not shipped. |
| MPL-2.0, LGPL-3.0 (psycopg) | Python dependencies | D-039: used unmodified; LGPL only for the psycopg driver family. |
| CKV_DOCKER_7 | `Dockerfile.go` | The base image is a build argument whose default is a version tag. |

## Open findings

1. **Go toolchain out of support.** The scorer image is built with Go 1.24.13; 22 standard-library advisories
   are fixed only in Go 1.25.9+ / 1.26.2+, and the fixed versions of pgx (critical), OpenTelemetry,
   `golang.org/x/{crypto,net,text}` and gRPC all require Go 1.25 or 1.26. The spec pins Go 1.24, so moving the
   toolchain is a maintainer decision (see PROGRESS.md "Needs the maintainer").
2. **govulncheck not run** here (network policy). It must run where `vuln.go.dev` is reachable (CI) before a
   release; until then trivy's module scan above stands in for it.
3. **Development database passwords.** `migrations/00001_roles.sql` creates `fraud_app` and `fraud_readonly`
   with development passwords; deployments must override them from the environment (deferred from the M1
   review).
