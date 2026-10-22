# ADR 0009: The scorer adds its own transaction to Redis instead of waiting for the feature-updater

## Status

Accepted.

## Date

2026-10-18

## Context

Several features count an account's earlier transactions: `tx_count_10m`, `tx_count_1h`, `tx_count_24h`,
`amount_sum_1h_minor` and `amount_ratio_24h`. Rule R003 (card testing) fires on eight or more transactions in
ten minutes, and card testing arrives in bursts, often concurrent. The feature-updater learns about a
transaction only from `events.transaction`, which the scorer publishes after it responds (ADR 0003), so it lags
by at least a Kafka round trip. If `Score` only read features, a burst would be scored against a history that
does not yet contain its own earlier members. Waiting for the updater instead would put Kafka on the scoring
path. The rule is: "`score_tx.lua` reads features strictly before `as_of`, then adds the transaction,
atomically; the updater's later `ZADD` of the same `event_id` is a no-op".

## Decision

- **One script, read then write.** `internal/features/score_tx.lua` reads the account's windows strictly before
  `as_of` (`score_tx.lua:28-48`) and then, in the same call, adds the transaction to `fr:{acc}:tx` (member
  `event_id` and amount, score `event_ts` in ms), records the device in `fr:{acc}:dev` (`ZADD GT`, last
  sighting) and `fr:{acc}:devfirst` (`ZADD LT`, first sighting), records the MCC in `fr:{acc}:mcc`, and trims
  each key to its retention (`score_tx.lua:50-67`). The scorer calls it once per `Score`, within 8 ms
  (`internal/features/store.go:121-153`, `internal/api/scorer.go:152-161`).
- **One slot.** Every per-account key carries the hash tag `{account_id}`, so the script touches one Redis
  slot.
- **The updater converges on the same state.** `apply_event.lua` writes the same member with the same score for
  the transaction the scorer already added, which changes nothing; `ZADD GT` and `ZADD LT` make the sighting
  updates order-independent (`internal/features/apply_event.lua`, ADR 0001).
- **Event time, bounded.** `as_of` is the request's `event_ts`, so replays compute features as of the event.
  Because that timestamp drives writes, `Score` accepts it only within [now − 24 h, now + 5 min]
  (docs/DECISIONS.md D-041).

## Alternatives considered

- **Wait until the feature-updater has applied earlier transactions.** Read-your-writes through Kafka offsets
  puts the broker and the updater's lag inside the 45 ms deadline.
- **Read only, let the updater write.** Concurrent members of a burst would not see each other, and the
  10-minute counts would lag by the updater's delay.
- **A read call followed by a separate write call.** Two round trips, and two concurrent requests for one
  account could both read before either writes.

## Consequences

Positive:

- One round trip per `Score`, atomic under concurrency (`TestReadAsOfThenAddIsAtomic`: one round trip per
  call, 64 concurrent calls). Only earlier events count, including out-of-order arrivals
  within 5 minutes (`TestOutOfOrderWithinFiveMinutes`).
- Kafka and the updater are off the scoring path; if `events.transaction` is dropped or late, the
  transaction's own features are already in Redis.
- Online and offline features agree exactly: 3,502 transactions × 13 features, 0 mismatches
  (`make feature-parity`).

Negative:

- The transaction is written before the decision is persisted. If the commit then fails outright (`Score`
  answers `UNAVAILABLE` because even the spool failed), the transaction stays in the features without a
  decision.
- A replay whose Redis idempotency value was lost runs the script again before Postgres reports the conflict;
  a replay with other contents adds its event even though it is then refused (D-064; `docs/KNOWN_LIMITATIONS.md`,
  "A replay after a lost idempotency value adds one feature event").
- Two writers maintain the same keys, so `score_tx.lua` and `apply_event.lua` must stay in step; parity and the
  Redis integration tests are the guard.
- Order matters at the edges. If transaction events are replayed into the updater before they are scored, each
  transaction's own sighting is already stored at its `as_of`, which the strictly-before read excludes, so its
  device and MCC read as novel (`docs/KNOWN_LIMITATIONS.md`, "Self-sighting order"). Out-of-order device and MCC
  sightings stay approximate because `fr:{acc}:dev` and `fr:{acc}:mcc` keep only the latest sighting
  (`docs/KNOWN_LIMITATIONS.md`, "Features").
- Every `Score` is a Redis write as well as a read; under load, Redis missing its 8 ms budget is what degraded
  99.3% of decisions in the laptop bench profile (`make bench BENCH_PROFILE=laptop`, `docs/KNOWN_LIMITATIONS.md`).
