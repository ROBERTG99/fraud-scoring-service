# Known limitations

What is not done, approximate or out of scope. Each entry names where it shows up and how it is bounded.

## Features

- **Out-of-order device and MCC sightings are approximate.** `fr:{acc}:dev` and `fr:{acc}:mcc` keep only the
  latest sighting per member (`ZADD GT`, spec §4.4). If an event later than a transaction arrives before the
  transaction is scored, that device or MCC looks unseen as of the transaction: `distinct_devices_24h` can
  under-count and `new_device` / `mcc_novel_30d` can read 1 instead of 0. A later event is never counted
  (`TestOutOfOrderWithinFiveMinutes`). Transaction windows and credential changes are exact.
- **Credential-change history is bounded.** Each `fr:{acc}:cred` field keeps the 16 most recent change times
  (91-day TTL). More than 16 changes of one kind within 91 days, arriving out of order, would make
  `minutes_since_*` approximate. The synthetic data never exceeds 2.
- **Self-sighting order.** The scorer adds a transaction to Redis before the feature-updater replays the same
  event from `events.transaction`; the replay is a no-op. If an operator replays transaction events into the
  updater *before* scoring them, device/MCC features for those transactions read as novel.
- **Account timelines are best effort.** `account_events` rows are written asynchronously by the
  feature-updater; when its queue is full or an insert fails, rows are dropped and counted
  (`fraud_account_events_dropped_total`, `fraud_account_events_insert_failed_total`). Decisions and Redis
  features do not depend on this table.

## Data and model

- All data is synthetic (`cmd/gen`, seed 7). Fraud patterns were designed to be learnable from the 13
  features; metrics on this data say nothing about production fraud (see `cmd/gen/PATTERNS.md`).
- Isotonic calibration is post-processed to a maximum slope of 10 (docs/DECISIONS.md D-008), which slightly
  smooths very steep steps of the calibration map.

## Serving

- **Triton was not run in the build environment.** `nvcr.io/nvidia/tritonserver` is blocked by the network
  policy (pull returned `Forbidden` on 2026-10-16). The `triton` compose profile, model repository
  (`serving/triton/`) and KServe v2 client (`internal/model/triton.go`, `TestKServeV2RequestShape` against a
  recorded request/response) exist, but `TestTritonGoldenVector` skips unless `FRAUD_TEST_TRITON_URL` points at
  a running Triton (`make triton-up`). `make verify` never requires Triton.

## Model server throughput on a shared host

The Python model server (BentoML) is the scorer's throughput limit when everything runs on one 4-CPU
machine. Measurements in docs/DECISIONS.md D-024: without request batching, 250 tx/s with shadow scoring
degraded 40% of decisions to rules-only (`MODEL_UNAVAILABLE`). The batching client fixed that for the seed
rate (68 of 16,863 degraded in the run after the D-025 fix); a run on a host already loaded by earlier runs
measured 5% degraded. Higher rates (the 1,000 RPS load target) need more model-server workers or Triton.

## Few review cases on the synthetic data

With point-in-time-correct features (D-025) the demo model separates the synthetic fraud almost perfectly:
replaying the 72 h test split gives 151 of 174 frauds DECLINE (score ≥ 0.85), 5 REVIEW and 18 APPROVE, so
only 10 cases are opened at the spec thresholds (review 0.30, decline 0.85). Measured with
`scripts/seed_host.sh` (seed exits 1 on its ≥ 50 open-case gate).

## `new_device` is almost always 0

`new_device` counts any earlier sighting of the device on the account, logins included
(`features/spec.yaml`). The generator's account-takeover and mule patterns log in on the new device before
transacting, so in the demo data `new_device=1` occurs on 14 of the 95,717 train and test transactions
(offline `training.features.compute_features`; all 14 are card testing) and rule R001 (`NEW_DEVICE_AFTER_CREDENTIAL_CHANGE`) practically never fires,
although `cmd/gen/PATTERNS.md` lists `new_device` as a signal for those patterns.
