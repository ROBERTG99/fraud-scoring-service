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

## Device features under 90-day gaps

`new_device` (spec v2) reads the device's first sighting on the account, kept in `fr:{acc}:devfirst` until
its last sighting leaves the 90-day retention. The offline mirror (`training/features.py`) uses the first
sighting in the data; the two only differ for a device idle for more than 90 days, which the 24-day
synthetic data never contains. `distinct_devices_24h` still keeps only each device's latest sighting
(`ZADD GT`), so a sighting that arrives before an earlier transaction is scored hides that device for it;
replay avoids this by scoring in per-account event order (D-025).
