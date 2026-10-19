# fraud-scoring-service

Real-time card-fraud scoring. A Go gRPC scorer decides each transaction (APPROVE, REVIEW or DECLINE) from
point-in-time features in Redis, a calibrated gradient-boosted model served over ONNX, and versioned rules with
reason codes. A separate Python worker writes LLM case summaries for analysts, and is kept off the scoring path:
the LLM can never change, delay or block a decision. All data is synthetic.

> **Status.** Milestones M0 to M8 of the [build specification](docs/specs/000-build-spec.md) are done, and M9's
> durability and chaos tests: scoring, features, model, rules, persistence, the case summarizer, the analyst
> console, evaluation, tracing, the security review, the compose stack, the SageMaker Terraform module, CI and the
> scripted demo. Open items are in [`PROGRESS.md`](PROGRESS.md) and
> [`docs/KNOWN_LIMITATIONS.md`](docs/KNOWN_LIMITATIONS.md).

## What it does

- **Scores** a card transaction in a single gRPC (or HTTP) call within a 45 ms budget, and falls back to
  rules only when the model or Redis is slow, so every call still gets a decision.
- **Explains** each decision with reason codes from the rules and the model, and returns the feature values it
  scored on when the caller asks (`explain`).
- **Persists** every decision before answering, through a Postgres group-commit writer, a disk spool for
  outages and an outbox to Kafka; duplicate calls get the stored decision back.
- **Summarises** REVIEW cases for analysts with an LLM, after redacting PII and fencing third-party text; a
  faithfulness check drops any statement the facts do not support.
- **Shows** the queue, case detail, timeline, shadow-model comparison, live metrics and rules in a React
  console.
- **Audits** analyst actions in a keyed, hash-chained log with anchors held outside the database.

## Results

All numbers below are offline, on synthetic data (generator seed 7), as recorded in
[`evals/REPORT.md`](evals/REPORT.md) and [`PROGRESS.md`](PROGRESS.md). The eval suites ran at commit 2eba265;
`make eval` does not re-run the benchmarks, so the latency rows are from earlier runs, as noted.

| Area | Result | Source |
|---|---|---|
| Model + rules | recall 0.9161 at 0.79% false-positive rate on the temporal test split (gate: ≥ 0.80 at ≤ 1.5%) | [`evals/REPORT.md`](evals/REPORT.md), `make eval-model` |
| Shipped REVIEW threshold | recall 0.8387 at 0.32% false-positive rate | [`evals/REPORT.md`](evals/REPORT.md), `make eval-model` |
| Calibration | ECE 0.0007 (gate ≤ 0.02); PR-AUC 0.8740 | [`evals/REPORT.md`](evals/REPORT.md), `make eval-model` |
| Decisions | 90 of 90 golden test cases exact on action and reason codes | [`evals/REPORT.md`](evals/REPORT.md), `make eval-decisions` |
| Summaries | schema-valid 1.0, faithfulness 1.0, pattern accuracy 0.9028, PII to provider 0 (offline FakeLLM) | [`evals/REPORT.md`](evals/REPORT.md), `make eval-summary` |
| Score latency | p50 11.3 ms, p95 19.6 ms, p99 30.5 ms at 300 requests/s for 60 s (budget p95 80 ms; run of 2026-10-17 at f9ab2a5) | [`evals/REPORT.md`](evals/REPORT.md), `make bench` |
| Model server | p95 4.63 ms per prediction (measured on the first model version, fraud-gbdt/1) | [`PROGRESS.md`](PROGRESS.md) T-16, `make bench-model` |
| Rules engine | 14.9 µs per 64-rule evaluation | [`PROGRESS.md`](PROGRESS.md) T-19, `go test -bench BenchmarkEval64Rules ./internal/rules` |
| Tests | Go core coverage 86.4%, Python 93.11%, 665 pytest and 67 Vitest tests | [`docs/verification.md`](docs/verification.md), `make test` |

Live LLM evals have not been run: there was no provider key in the build environment
([`evals/REPORT.md`](evals/REPORT.md)). How each suite works is in [`docs/evaluation.md`](docs/evaluation.md).

## Architecture

```mermaid
flowchart LR
    client([Caller]) -->|Score, gRPC/HTTP| scorer[Go scorer]
    scorer -->|Lua, 8 ms| redis[(Redis features)]
    scorer -->|ONNX, 20 ms| model[BentoML model server]
    scorer --> rules[YAML rules]
    scorer -->|group commit, 30 ms| pg[(Postgres)]
    pg -->|outbox| kafka[[Kafka]]
    events[[Account events]] --> updater[Feature updater] --> redis
    kafka --> summarizer[Python case summarizer]
    summarizer -->|redacted, fenced facts| llm[LLM provider]
    summarizer --> pg
    console[React console] --> webserve[webserve proxy]
    webserve --> scorer
    webserve --> summarizer
```

The scorer never waits on the summarizer or the LLM; summaries are written asynchronously for REVIEW
decisions only. The full design, the Score sequence and the degraded modes are in
[`docs/architecture.md`](docs/architecture.md); the reasoning behind each choice is in the ADRs in
[`docs/adr/`](docs/adr/README.md) and the answers in [`docs/design-faq.md`](docs/design-faq.md).

## Screenshots

Captured by the end-to-end suite (`E2E_SCREENSHOTS=1`, PROGRESS T-35) against the host stack.

![Review queue](docs/images/queue.png)
![Case detail with the AI summary](docs/images/case-detail.png)

More in [`docs/images/`](docs/images/): timeline (captured before the amount-display fix, so it shows
amounts 100 times too large; PROGRESS), shadow comparison, live metrics, rules, the label dialog and
the Grafana dashboard.

## Quick start

Requirements: Docker with Compose v2. Everything runs offline; no API key is needed.

```bash
make up      # docker compose up -d --build --wait: every service healthy
make seed    # generate the demo data (seed 7), train and register the model, replay the last 72 h
```

The console is then on <http://localhost:8500>; sign in by picking a synthetic persona from the dev issuer (an
analyst can label cases). Without make: `docker compose up -d --build --wait` and
`docker compose run --rm seed`. From empty volumes on a laptop (Docker Desktop, macOS arm64) `make up` took
94 s and `make seed` 104 s, once the images were built (`PROGRESS.md`, T-43). `make down` stops the stack;
`docker compose down -v` also deletes its data.

For development, the toolchains come from `make bootstrap` (Go, Python with uv, Node with pnpm; see
[`docs/DEVELOPMENT.md`](docs/DEVELOPMENT.md)), and `FULL=1 scripts/seed_host.sh` runs the same services on the
host against `compose.test.yaml` (console on <http://127.0.0.1:18500>; the script's header says how to stop
them).

## Demo

With the stack from the quick start running (compose or host), `make demo` plays the spec's user journeys
offline in about ten seconds. Each run uses fresh accounts.

1. **Analyst.** A USD 940 online purchase comes from a new device two minutes after an email change. It is
   REVIEWed with `NEW_DEVICE_AFTER_CREDENTIAL_CHANGE`, summarized as an account takeover with three verified
   evidence items, and queued. The analyst then labels it fraud in the console.
2. **Risk engineer.** The shadow model's comparisons are recorded beside the decisions.
3. **Degradation.** The model server is stopped: decisions continue rules-only, and the live metrics show
   `MODEL_UNAVAILABLE`. The server is then started again.

The run also covers an idempotent replay, a card-testing burst and an invalid event in the DLQ. Every step is
a checkpoint, and the demo fails unless all of them pass. The checkpoint lines of a run on 2026-10-19
(decision and case IDs shortened):

```text
demo: OK   1/10 ato_review (FR-1, FR-3, FR-7): USD 940 online, new device, email changed 2 min earlier: REVIEW in 8.0 ms, score 0.65, NEW_DEVICE_AFTER_CREDENTIAL_CHANGE,MODEL_SCORE_ELEVATED (model fraud-gbdt/3) (want REVIEW with NEW_DEVICE_AFTER_CREDENTIAL_CHANGE in < 50 ms)
demo: OK   2/10 idempotent_replay (FR-1): same transaction_id scored twice: decision 01a1526d…a97b, then 01a1526d…a97b
demo: OK   3/10 card_testing_burst (FR-7): eight USD 1.50 tests in 10 min, then USD 2.00: DECLINE in 4.2 ms, score 1.00, CARD_TESTING_BURST,MODEL_SCORE_HIGH (model fraud-gbdt/3) (want CARD_TESTING_BURST)
demo: OK   4/10 invalid_event_dlq (FR-2): login event without its required fields (events.login partition 3 offset 15785) is in events.dlq
demo: OK   5/10 case_persisted (FR-10, FR-13): decision 01a1526d…a97b is open case 82d34c4c…0bfb in the review queue
demo: OK   6/10 summary_verified (FR-11): ok after 0.0 s: pattern account_takeover, 3 of 3 evidence items verified, faithfulness 1.00, model fake-case-summary-v1; decision unchanged: true
demo: OK   7/10 shadow_compared (FR-9): 16901 comparisons in 24 h, shadow fraud-gbdt/3 against primary fraud-gbdt/3, agreement 100.0%
demo: OK   8/10 model_unavailable_rules_only (FR-8): model server stopped: 20 of 20 decided rules-only (MODEL_UNAVAILABLE, model none), slowest 28.2 ms (budget 45), USD 250 → REVIEW
demo: OK   9/10 live_metrics_degraded (FR-8, FR-13): live metrics (last 10 s): 18 MODEL_UNAVAILABLE, degraded rate 64%
demo: OK   10/10 model_recovered (FR-8): model server back: decision 1 used model fraud-gbdt/3 after 0.0 s (REVIEW in 5.2 ms, score 0.56, MODEL_SCORE_ELEVATED (model fraud-gbdt/3))
demo: OK   11/13 console_case (FR-13, console): the analyst opens the demo case with its reason codes and AI summary
demo: OK   12/13 evidence_highlights_fact (FR-13, console): selecting an evidence item highlights its fact
demo: OK   13/13 label_leaves_queue (FR-13, console): the analyst labels the case fraud after the row preview
demo: all 13 checkpoints OK (artifacts/demo/checkpoints.json)
```

The shadow comparison runs the production version against itself until a second version is registered. The
scenario places the purchase in the early afternoon UTC, because the model weighs the hour of day.
[`docs/DECISIONS.md`](docs/DECISIONS.md) D-071 and [`docs/KNOWN_LIMITATIONS.md`](docs/KNOWN_LIMITATIONS.md)
have the details.

To check the code without running the stack:

```bash
make check-fast   # lint and unit tests, under a minute
make test         # unit, integration and contract tests (starts the compose.test.yaml services)
make eval         # every eval suite and evals/REPORT.md
make security     # gitleaks, gosec, govulncheck, bandit, pip-audit, pnpm audit, hadolint, checkov, trivy
```

## Repository layout

| Path | What |
|---|---|
| `cmd/` | scorer, feature-updater, gen, replay, seed, dev-issuer, webserve, retention, benchreport, loadgen |
| `internal/` | API handlers and interceptors, features (Lua), model clients, rules, decisions, store, stream, auth, telemetry |
| `proto/`, `gen/` | protobuf sources; generated Go, gateway and OpenAPI |
| `training/`, `serving/` | dataset, offline features, training, ONNX export, evaluation, registry; BentoML and Triton |
| `src/fraud_summarizer/` | case summarizer: consumer, pipeline, redaction, faithfulness, LLM gateway |
| `prompts/`, `evals/` | versioned prompts; eval datasets, suites, graders and the report |
| `web/` | React + Vite analyst console |
| `migrations/`, `deploy/` | goose SQL migrations; OpenTelemetry, Prometheus and Grafana configuration |
| `tests/` | unit, integration, contract, end-to-end, harness and LLM tests |

## How it was built

A coding agent built this repository from the written specification, milestone by milestone, test first, with
subagent reviews at the end of each milestone and a security audit that took nine rounds. How the harness was
used, and the real mistakes made along the way, are in [`docs/ai-development.md`](docs/ai-development.md).
Every decision is recorded in [`docs/DECISIONS.md`](docs/DECISIONS.md).

## Documentation

| Document | Contents |
|---|---|
| [`docs/architecture.md`](docs/architecture.md) | Components, the Score path, degraded modes, data flow |
| [`docs/design-faq.md`](docs/design-faq.md) | Fifteen design questions answered with file references |
| [`docs/adr/`](docs/adr/README.md) | Architecture decision records |
| [`docs/evaluation.md`](docs/evaluation.md) | Eval suites, gates and current results |
| [`docs/security.md`](docs/security.md) | Threat model, controls, scan results, open findings |
| [`docs/runbook.md`](docs/runbook.md) | Alerts, audit verification, credentials, key rotation |
| [`docs/KNOWN_LIMITATIONS.md`](docs/KNOWN_LIMITATIONS.md) | What does not work, or works only partly |
| [`docs/verification.md`](docs/verification.md) | The latest verification run |
| [`docs/DEVELOPMENT.md`](docs/DEVELOPMENT.md) | Build environment and network notes |
| [`CONTRIBUTING.md`](CONTRIBUTING.md), [`SECURITY.md`](SECURITY.md), [`CHANGELOG.md`](CHANGELOG.md) | Contributing, reporting vulnerabilities, changes |

## Limitations

The main ones: offline evals only (no live LLM run), synthetic data only, Triton never run, and timing tails of
the compose stack on a laptop. The full list is in
[`docs/KNOWN_LIMITATIONS.md`](docs/KNOWN_LIMITATIONS.md) and the open security findings in
[`docs/security.md`](docs/security.md).

## License

[MIT](LICENSE). Part of an AI engineering portfolio by [@ROBERTG99](https://github.com/ROBERTG99).
