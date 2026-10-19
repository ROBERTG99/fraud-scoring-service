# Runbook

What each alert means and what to check first. Alerts are defined in `deploy/prometheus/alerts.yml`
(unit-tested with `make obs-check`) and shown on the Grafana dashboard **Fraud scoring**
(`deploy/grafana/fraud.json`, profile `observability`, <http://localhost:8590>). Traces are in Jaeger
(<http://localhost:8592>): every decision has a `fraud.score` span, and a REVIEW decision's trace continues
into the case summarizer through the Kafka `traceparent` header.

A degraded or slow scorer never blocks a decision: Score answers within 45 ms with the rules-only fallback
(spec §4.7). The alerts say the system is working in a worse mode, not that it stopped deciding.

## ScoreLatencyHigh

**Fires when** the `Score` p95 is above 50 ms over 5 minutes, for 5 minutes (critical).

1. Dashboard **Score latency**: is p50 up too (the whole path is slow) or only p95/p99 (a dependency stalls)?
2. Jaeger, service `scorer`, operation `fraud.score`, sorted by duration: which child span is long?
   - `features.score_tx` (Redis, budget 8 ms): Redis latency or CPU on the Redis host.
   - `model.predict` (model server, budget 20 ms): model-server CPU or worker count; see
     `docs/KNOWN_LIMITATIONS.md`, "Model server throughput on a shared host".
   - `postgres.commit` (budget 30 ms): Postgres; the writer's breaker spools when commits fail (D-027).
3. Host load: the CI bench degrades on a busy host (`docs/KNOWN_LIMITATIONS.md`); compare with
   `make bench` on an idle machine.

## DegradedDecisions

**Fires when** more than 1% of decisions are degraded (rules only) over 10 minutes, for 10 minutes (warning).

1. Dashboard **Degraded decisions by reason**: `model` (model server unavailable or past its 20 ms
   deadline) or `features` (Redis unavailable or past 8 ms).
2. `model`: check the model server's `/readyz` (port 8514) and its log; the scorer recovers on its own
   once `/predict` answers within the deadline again.
3. `features`: check Redis (port 8530) and the **Feature updater lag** panel; a lagging updater does not
   degrade decisions by itself, but Redis timeouts do.
4. A burst at start-up is expected while caches warm (the seed's first seconds); a sustained rate is not.

## OutboxUnsentAge

**Fires when** the oldest unsent row of `decision_outbox` is older than 60 s, for 1 minute (warning).

1. Dashboard **Outbox and spool backlog**: a growing `outbox unsent` means the relay cannot publish to
   Kafka; a growing `spool depth` means Postgres commits are failing and decisions wait on disk.
2. Kafka (Redpanda, port 8540): broker health and the `decisions` topic. The relay retries every interval
   and counts attempts per row (`decision_outbox.attempts`).
3. Decisions are not lost: they are committed in Postgres (or the spool) before the response, and the relay
   publishes them (at least once) when Kafka is back; consumers deduplicate on `decision_id`.

## Summaries in the DLQ

There is no alert yet; `fraud_summarizer_messages_total{outcome=~"exhausted|failed|invalid"}` counts what the
case summarizer parks in `events.dlq` (`make dlq-peek` shows the records). A parked REVIEW decision keeps its
case, which the console shows without an AI summary.

1. `invalid`: the message failed its JSON Schema; the `error` field names the violation.
2. `failed`: handling failed 5 times for a reason other than an unreachable dependency (a bug, bad data);
   the `error` field has the last exception. Fix the cause before replaying.
3. `exhausted`: the decision used its 3 paid summary attempts (`summary_attempts`, D-048), so something after
   the provider call keeps failing. Fix it, then `DELETE FROM summary_attempts WHERE message_key = '<decision_id>'`
   (migration role) before replaying, or regenerate the summary from the console.
4. A spent daily LLM budget (`FRAUD_LLM_DAILY_BUDGET_USD`, default 25 USD) parks nothing: summaries are
   templated (`status = fallback`) until the next UTC day, and `fraud_llm_calls_total{outcome="budget"}` rises.

## Audit log verification

`make audit-verify` (`scorer audit-verify`) recomputes the `audit_log` chain under `FRAUD_AUDIT_KEY` (D-050)
and checks every anchor in `FRAUD_AUDIT_ANCHORS`; it exits 1 when the chain is broken, naming the first bad
`seq`, and 2 when it cannot run. Outside `FRAUD_ENV=dev` it refuses to run without an anchors file, because a
chain with its newest entries cut off still verifies on its own (D-057). Schedule `scorer audit-anchor`
(hourly, say): it verifies the chain and appends the head's `seq` and hash, signed with the key and chained to
the previous anchor. Keep the anchors file on storage the database's operators cannot rewrite or truncate (an
append-only bucket, a separate host): the chain of anchors shows a line removed from the middle, but not the
newest lines removed. The compose stack keeps it on the `audit-anchors` volume, which is only a stand-in.
Entries written before migration 00010 (unkeyed) verify only up to `FRAUD_AUDIT_LEGACY_MAX_SEQ` (default 0,
none); set it once, from the last unkeyed `seq` at upgrade time, and keep it outside the database.

1. `hash mismatch` or `prev_hash does not match`: an entry was changed or inserted outside the scorer. The
   triggers stop every role but the table owner (the migration role) and superusers; check who used them.
2. `unkeyed entry after keyed entries` or `beyond FRAUD_AUDIT_LEGACY_MAX_SEQ`: someone wrote old-format rows to
   avoid the key.
3. `entries were removed` or `missing from the chain`: rows after the last anchor's `seq` were cut off.
4. `MAC does not match the audit key`: the anchor file was edited, or the key differs from the one that wrote it.
5. `anchor chain broken`: a line was removed from the anchors file or the lines were reordered.

Anchors written before D-057 (no `prev` field) fail with "MAC does not match": start a new anchors file with
`scorer audit-anchor` after verifying the chain once without it in dev mode.

## Database credentials outside dev mode

Outside `FRAUD_ENV=dev` the scorer, `scorer migrate`, `audit-verify` and the case summarizer judge the
credentials the driver would use (D-059, D-063, D-064): a URL or keyword/value DSN (`FRAUD_PG_DSN`,
`FRAUD_PG_MIGRATE_DSN`, `FRAUD_SUMMARIZER_PG_DSN`), its query parameters, `PGUSER` and `PGPASSWORD`, the
matching passfile entry and, in the summarizer, a libpq service file. They refuse to start with no password
(unless a client certificate is configured and TLS is on) or with the migrations' development password, which
is the role's own name. Exit code 2 and the role name in the log mean: set a real password from a secret.

## Model pin outside dev mode

Outside `FRAUD_ENV=dev` the model server loads a local (`FRAUD_MODEL_DIR`) or cached model only when
`FRAUD_MODEL_SHA256_<ALIAS>` (for example `FRAUD_MODEL_SHA256_PRODUCTION`, upper case alias) holds its
SHA-256 (D-063): a `model_meta.json` next to the file cannot vouch for it. Set one pin per alias the server
serves, from the registry version you promote (`make register-model`). The cache fallback of spec §4.7 (MLflow
down) also needs the pin; without it the server stays not-ready and the scorer decides rules-only, which is
the safe failure. `scripts/bench_model.py` and the offline evaluation serve a just-trained model and run in
dev mode or with `require_pin=False`.

## Key rotation

`FRAUD_AUDIT_KEY` keys the audit chain and, through a derived key, the Redis idempotency values (D-050,
D-062). Every scorer replica must run the same key: a replica with another key reads the others' idempotency
values as forged and deletes them. Changing the key is not supported for the audit chain in this version:
entries written under the old key verify only under it (`docs/KNOWN_LIMITATIONS.md`). If the key leaks, stop
the scorers, run `scorer audit-anchor` and `audit-verify` under the old key, archive the anchors file and the
verification output with the old key, and start a new database or a new chain under the new key. The Redis
values need nothing: under a new key they read as misses, and the first retry of each transaction finds the
decision in Postgres and writes its value again (D-064).
