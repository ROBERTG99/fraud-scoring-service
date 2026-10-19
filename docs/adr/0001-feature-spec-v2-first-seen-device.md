# ADR 0001: Feature spec v2, "new device" means first seen less than 24 h ago

## Status

Accepted.

## Date

2026-10-17

## Context

The model input contract is `features/spec.yaml` (13 features, read by the Go scorer and the Python
trainer). In spec v1, `new_device` was 1 only when the device had no sighting at all on the account in 90
days. A successful login counts as a sighting, and an account takeover logs in on the attacker's device
before buying, so `new_device` was 0 for nearly every takeover and mule transaction in the synthetic data
(14 of 95,717 train and test transactions had `new_device=1`). Rule R001 (`new_device` within 60 minutes
of a credential change) could practically never fire, although the product journey (spec §3.4) and
`cmd/gen/PATTERNS.md` rely on it. The v1 value was also order-sensitive online: it read the device's
latest sighting (`ZADD GT`), so a later sighting applied early made an old device look new
(docs/DECISIONS.md D-025).

## Decision

Spec v2 defines `new_device = 1` unless the device's first sighting on the account, within the 90-day
retention, is earlier than `as_of − 24 h`.

- Online: a per-account ZSET `fr:{acc}:devfirst` keeps each device's first sighting with `ZADD LT`. It is
  written by `apply_event.lua` and `score_tx.lua`, and a device is removed from it when its last sighting
  leaves the 90-day retention of `fr:{acc}:dev`.
- Offline: `training/features.py` uses the earliest sighting before the transaction; the naive reference
  implementation and the property tests mirror it.
- The spec version is checked everywhere it matters. `features.ParseSpec` requires v2. The scorer refuses
  a rule set pinned to another `feature_spec`, at startup and on hot reload. The model server refuses an
  artifact whose `model_meta.json` declares another `feature_spec` and stays not-ready (the scorer
  degrades to rules-only).
- Rules are re-pinned as `rules/rules.v2026.10.2.yaml` (same rules, `feature_spec: v2`).

## Alternatives considered

- Keep v1 and change the generator so attackers transact without logging in: hides the problem instead
  of fixing the feature, and real attackers do log in.
- Count only transactions as sightings: a legitimate customer's first purchase on a phone they have used
  to log in for months would look new.
- Track the full sighting history: exact, but unbounded state per account; first and last sighting give
  the two features we need in constant space.

## Consequences

- The minimum is order-independent, so `new_device` is exact under any arrival order (the property test in
  `internal/features/redis_test.go` now asserts equality, not a bound).
- Every model must be retrained on spec v2; models registered from v1 are refused by the model server.
- A device idle for more than 90 days and seen again counts as new online, while the offline mirror uses
  the first sighting in the data; the 24-day synthetic data never contains such a gap
  (docs/KNOWN_LIMITATIONS.md).
- Parity was re-verified: `make feature-parity` → 3,502 transactions × 13 features, 0 mismatches.
