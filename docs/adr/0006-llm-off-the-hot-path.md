# ADR 0006: The LLM writes case summaries asynchronously and never touches a decision

## Status

Accepted.

## Date

2026-10-18

## Context

Analysts used to rebuild each review case from raw event logs. A language model can summarize a
case's facts into a pattern, cited evidence and checks to run. But a provider call takes seconds (the route's
timeout is 8 s), costs money, can fail or refuse, returns non-deterministic text, and reads input that an
attacker controls in part: merchant names reach the facts. `Score` has a 45 ms deadline (ADR 0002). `CLAUDE.md`
puts it as a hard rule: the LLM may never change, delay or block a decision, and nothing other
than the model server, Redis and Postgres may be called on the scoring path.

## Decision

- **Separate process, separate language.** Summaries are written by the Python case summarizer
  (`src/fraud_summarizer/`). Only `src/fraud_summarizer/llm/` imports a provider SDK
  (`anthropic_adapter.py`, `openai_adapter.py`); the Go module has no LLM dependency.
- **Triggered after the decision is committed.** The scorer commits the decision and its outbox row before it
  responds (ADR 0007); the relay publishes `decisions`; the summarizer consumes it and handles REVIEW decisions
  only (`src/fraud_summarizer/consumer.py:102-126`). Nothing flows back to the scorer.
- **A fixed pipeline, not an agent.** `load_facts` → `redact` → `summarize` → `validate_schema` →
  `check_faithfulness` → `persist` (`src/fraud_summarizer/pipeline.py:206-398`). The model gets no tools. Facts
  are redacted before the call (`redact.py`) and fenced as data in the prompt (`prompts/case_summary.v1.md`);
  output is validated against the Pydantic schema, and an evidence item survives only if code finds its
  fact key and value in the facts (`faithfulness.py`). Otherwise the stored summary is a templated `fallback`
  or is marked `unfaithful`.
- **A privilege boundary, not only a convention.** The `fraud_summarizer` Postgres role can read decisions
  and cases but write only `case_summaries`, `step_records`, `processed_messages` and its attempt and
  regeneration tables (`migrations/00007_service_roles.sql`, docs/DECISIONS.md D-043,
  `TestServiceRolesAreLeastPrivilege`).
- **Bounded cost.** At most 3 paid attempts per decision (`summary_attempts`), a daily budget per process, and
  a 20-per-hour per-user limit on regenerate (D-048, D-054). Retries follow D-014 (only retryable status codes).
- **Offline by default.** `FRAUD_SUMMARY_PROVIDER=fake` selects FakeLLM; evals and tests run without a key.

## Alternatives considered

- **Call the LLM inside `Score` with a short timeout.** Even a timed-out call would spend the budget the
  decision needs, and a provider outage would become a scoring incident.
- **Summarize synchronously when an analyst opens the case.** Puts seconds of provider latency behind the
  console's 2 s budget (`internal/api/cases.go:23`) and pays per view rather than per case.
- **Let the model propose the action or adjust the score (an agent with tools).** Excessive agency over money
  movement, and decisions would no longer be reproducible from features, rules and a model version.

## Consequences

Positive:

- Score latency and availability do not depend on any provider. A provider outage produces `fallback`
  summaries (`test_provider_failure_writes_fallback`) and leaves every decision as it was.
- What reaches the provider is measurable: on the summaries eval, planted PII reaching the provider is 0 and
  faithfulness is 1.0 (`make eval-summary`, `evals/REPORT.md`); `test_no_pii_reaches_provider` checks every
  route.
- The trace of a summary joins its decision's trace through the Kafka `traceparent` header (D-038).

Negative:

- A summary arrives after the case exists: relay polling (100 ms, `cmd/scorer/main.go:209`), consumption and
  up to 8 s per provider attempt. An analyst may open a case before its summary is written.
- More moving parts: a Kafka consumer, a DLQ for invalid or exhausted messages, retry state held in memory
  (D-048).
- The daily budget is per process, so several summarizer replicas multiply it (`docs/KNOWN_LIMITATIONS.md`,
  "LLM cost cap is per process").
- Live quality is unmeasured: there is no provider key here, so every summary number is offline, with FakeLLM
  tuned on the dev split (`evals/REPORT.md`, D-032). The useful-summary judge's offline agreement (0.900) reuses
  FakeLLM's classifier (`make eval-judge`, `evals/REPORT.md`).
- Code-checked faithfulness drops natural phrasing that does not quote the fact's value
  (`docs/KNOWN_LIMITATIONS.md`, "Faithfulness drops natural phrasing").
