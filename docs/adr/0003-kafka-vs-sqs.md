# ADR 0003: Kafka protocol (Redpanda) for events and decisions, not SQS

## Status

Accepted.

## Date

2026-10-18

## Context

Three kinds of events change an account's features: logins, device changes and transactions. Point-in-time
features are only correct if each account's events are applied in order (docs/DECISIONS.md D-025 shows what
happens otherwise: 8,970 of 16,863 decisions had `new_device=1` against 4 point-in-time, on generator v1 data). Decisions must reach
the case summarizer at least once, from an outbox (ADR 0007). The feature-updater and the summarizer consume
independently, history must be replayable for seeding and tests, and the whole system must run offline on a
laptop without a cloud account. The choice is Redpanda with franz-go; SQS is the rejected
alternative.

## Decision

All asynchronous traffic uses the Kafka protocol: Redpanda locally (`compose.test.yaml`), franz-go in Go
(`internal/stream`, with `kfake` in tests) and aiokafka in the summarizer.

- Topics `events.login`, `events.device`, `events.transaction`, `decisions` and `events.dlq`, created with 12
  partitions (`internal/stream/kafka.go:15-18`), replication 1 locally (`cmd/feature-updater/main.go:87`).
- Every message is keyed by `account_id`, so one account's events stay in one partition, in order. Values
  are JSON validated against `schemas/events/` in Go and Python; invalid messages go to `events.dlq` with the
  error, and the offset is committed (`cmd/feature-updater/updater.go:82-140`,
  `TestUpdaterRoutesInvalidToDLQ`).
- Consumers commit offsets only after their handler succeeds: the feature-updater per partition batch
  (`internal/stream/consumer.go:30-31`), the summarizer per processed prefix
  (`src/fraud_summarizer/consumer.py:208-264`).
- The outbox relay produces `decisions` with the Score span's W3C `traceparent` as a record header
  (`internal/store/outbox.go:75-79`, D-038).

## Alternatives considered

- **SQS standard queues.** No ordering guarantee, so per-account feature updates would need their own
  sequencing; a consumed message cannot be replayed.
- **SQS FIFO with `account_id` as the message group.** Ordering per account, but no replay from an offset,
  one queue per consumer (or SNS fan-out) for the updater and the summarizer, and an AWS account or an
  emulator for every local run and test.
- **Postgres as the queue (consumers poll the outbox directly).** One fewer system, but the feature-updater's
  event volume would load the database the scorer commits to within 30 ms, and the login and device events
  have no other reason to be in Postgres.

## Consequences

Positive:

- Per-account ordering comes from the key, which point-in-time features need; the replay uses it to warm
  history and then applies live events per account (`internal/replay/replay.go:1-15`).
- Consumer groups and offsets give each consumer its own position and let the seed and tests replay data.
- Tests run without a broker: `kfake` drives the relay crash test (`TestRelayCrashProducesDuplicatesWithSameDecisionID`)
  and the updater tests; integration tests use Redpanda from `compose.test.yaml`.
- Headers carry trace context, so a summary joins its decision's trace even when published minutes later
  (29 of 29 sampled traces, `scripts/obs_verify.py`).

Negative:

- A broker has to run. Locally it is one Redpanda node with replication 1; the compose service is `redpanda`
  in `compose.yaml`.
- Delivery is at least once, so every consumer deduplicates: ZSET members by `event_id`, `processed_messages`
  by `decision_id`, `ON CONFLICT DO NOTHING` inserts.
- There is no broker-side redelivery or visibility timeout. Retry and dead-lettering are written in each
  consumer: the summarizer seeks back with backoff from 1 s to 30 s, sends a message to the DLQ after 5
  non-transient failures or 3 paid attempts, and retries transient failures without a cap (D-048). While it
  retries, the rest of that partition waits. Its failure counters are in memory and restart with the process.
- The partition count is fixed at 12; adding partitions later moves keys between partitions.
- The scorer's `events.transaction` producer is non-blocking and drops records when its 100,000-record buffer
  is full (`cmd/scorer/publisher.go:17-27`, `fraud_publish_dropped_total`). Redis already holds the
  transaction (ADR 0009); only the updater's `account_events` timeline misses it.
