# ADR 0007: Group commit before responding, an outbox for publishing, a disk spool for outages

## Status

Accepted.

## Date

2026-10-18

## Context

A decision that was returned to the card platform must not be lost, and it must reach the case summarizer at
least once (FR-10). Postgres has a 30 ms budget inside the 45 ms Score deadline, and the load target is 1,000
RPS. One transaction per decision means one commit per request. Writing to Postgres and then producing to
Kafka from the handler is a dual write: a crash between the two loses the message or publishes a decision
that was never stored. And Postgres can be down or hung while scoring must go on (FR-8).

## Decision

- **Group commit.** `store.GroupWriter` (`internal/store/writer.go`) queues decisions (10,000 slots) and writes
  a batch of up to 256, or whatever arrived within 2 ms, in one transaction. Each record inserts the decision,
  its `decision_outbox` row and, for REVIEW, its `review_cases` row through one CTE
  (`internal/store/writer.go:265-279`). Callers wait for their batch to commit before `Score` responds
  (`Commit`, `writer.go:141-173`).
- **Idempotent inserts.** Every insert is `ON CONFLICT DO NOTHING` on `transaction_id`; conflicting records get
  the stored decision and its request hash back from the same transaction (`writer.go:326-348`,
  docs/DECISIONS.md D-017, D-057).
- **Back-pressure.** If no queue slot frees up within 5 ms, `Commit` returns `ErrOverloaded` and `Score`
  answers `RESOURCE_EXHAUSTED` (`writer.go:149-163`).
- **Spool and breaker.** When a batch fails or exceeds its 200 ms deadline, or a caller's 30 ms deadline passes,
  the decision goes to an fsynced JSONL spool (`internal/store/spool.go:82-109`) and the caller is answered. A
  failed batch opens a breaker: every later commit spools directly until the replay loop's ping succeeds,
  which closes the breaker at once; the backlog then drains in the background through the same idempotent
  insert (`writer.go:408-457`, D-027).
- **Transactional outbox.** The relay inside each scorer (`cmd/scorer/main.go:208-209`) polls every 100 ms,
  locks up to 500 unsent rows with `FOR UPDATE SKIP LOCKED`, produces them to `decisions` synchronously and
  marks them sent in the same transaction (`internal/store/outbox.go:47-106`). Consumers deduplicate by
  `decision_id` (`processed_messages`). The row also carries the Score span's `traceparent` (D-038).

## Alternatives considered

- **One transaction per `Score`.** Simpler, with one commit and one fsync per request instead of one per batch.
- **Respond first and write asynchronously.** Lowest latency, but a crash loses decisions the caller already
  acted on, which FR-10 forbids.
- **Produce to Kafka from `Score` (dual write).** Inconsistent on any crash between the two writes, and it puts
  Kafka on the scoring path.
- **Change data capture on `decisions` instead of polling an outbox.** No polling, but another component to run,
  and the message shape would be tied to the table.
- **Keep queueing behind a slow Postgres (spec §4.7 as first written).** Rejected in D-027: a hung Postgres
  turned 1,896 of 3,200 responses into `RESOURCE_EXHAUSTED` in `TestHungPostgresSpoolsInsteadOfOverloading`
  before the breaker existed.

## Consequences

Positive:

- A response means the decision is in Postgres or in the fsynced spool (`TestCommitBeforeRespond`). 500
  concurrent commits are written in batches (`TestGroupCommitBatchesConcurrentWrites`), and 20 decisions
  spooled during a cut connection are replayed once each, with their outbox and case rows
  (`TestSpoolReplayAfterPostgresRecovers`, `PROGRESS.md` T-25).
- Decision, outbox row and case are atomic. A relay crash after producing re-sends rows with the same
  `decision_id`, and the consumer processes each once (`TestRelayCrashProducesDuplicatesWithSameDecisionID`:
  10 messages for 5 decisions, 5 processed, `PROGRESS.md` T-26).
- A Kafka outage never reaches `Score`; unsent rows wait and are visible as `fraud_outbox_unsent` and
  `fraud_outbox_oldest_unsent_age_seconds` (alert `OutboxUnsentAge`).
- The commit is inside the measured latency: p95 19.6 ms at 300 rps (`make bench`, `evals/REPORT.md`).

Negative:

- Postgres is on the response path: a request waits for its batch (up to the 2 ms flush interval) and the
  commit.
- The spool is the scorer's local disk (compose volume `scorer-spool`). A decision in it is only as durable as
  that disk, and nothing has tested a kill under load: `TestDurabilityKill` (NFR-7) is T-48 and not written.
- While spooling, idempotency is weaker: with no valid Redis value, a replay with other contents is spooled as
  a new decision and answered with it; the spool replay later keeps the first (`docs/security.md`). In breaker
  mode each commit fsyncs on its own (D-027).
- Open non-blocking items from the M3 review: a half-hung Postgres reopens the breaker once per replay interval,
  and a `Commit`/`Close` race can reopen a spool file (`PROGRESS.md`, milestone reviews).
- Delivery is at least once, so every consumer must deduplicate, and the 100 ms polling interval adds to the
  time before a summary starts. Published outbox rows are deleted after 7 days (D-040).
