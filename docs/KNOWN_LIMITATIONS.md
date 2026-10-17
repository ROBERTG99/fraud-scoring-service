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
rate: 68 of 16,863 degraded on gen v1 data after the D-025 fix and 59 of 16,904 on gen v2 data
(`scripts/seed_report.py`); a v1 run on a host already loaded by earlier runs measured 5% degraded. Higher rates (the 1,000 RPS load target) need more model-server workers or Triton.

## Device features under 90-day gaps

`new_device` (spec v2) reads the device's first sighting on the account, kept in `fr:{acc}:devfirst` until
its last sighting leaves the 90-day retention. The offline mirror (`training/features.py`) uses the first
sighting in the data; the two only differ for a device idle for more than 90 days, which the 24-day
synthetic data never contains. `distinct_devices_24h` still keeps only each device's latest sighting
(`ZADD GT`), so a sighting that arrives before an earlier transaction is scored hides that device for it;
replay avoids this by scoring in per-account event order (D-025).

## Look-alike amounts carry no noise

`legitProtectedTx` (the v2 look-alike sequences in `cmd/gen`) sets amounts at an exact multiple of the
account median, without the lognormal noise ordinary transactions get. No feature reads the pattern, but a
model could in principle learn the exact ratios; adding noise changes the dataset, so it is left for a
generator v3.

## Eval sets and offline summaries

- **Offline pattern accuracy measures FakeLLM, not a real model.** `make eval-summary` runs the real pipeline
  with FakeLLM, a rule-based stand-in tuned on the dev split only (`src/fraud_summarizer/llm/fake.py`): test
  pattern accuracy 0.9028 (65/72, 95% CI 0.833–0.958, `evals/reports/summaries.json`), so the lower bound of
  the interval is under the 0.85 threshold. Faithfulness is 1.0 by construction, since FakeLLM quotes fact
  values verbatim. A real model's numbers come only from `make eval-live` (needs a provider key).
- **A hard subset is indistinguishable from the facts.** Legitimate splurges look like friendly fraud, and a
  first purchase of the day has no 24-hour baseline (`amount_ratio_24h = 1`); these cases are labelled with
  the generating scenario and count as misses for any summarizer that reads only the facts.
- **`new_device` is 0 on P2P cases from a recently bound device.** In `evals/datasets/summaries` (v1), the
  mule (`churn`, `slow`) and `legit_bill_split` cases bind the device 30–300 minutes before the transfer but
  carry `new_device = 0`; feature spec v2 would give 1. No label or reason code depends on it (R004 does not
  read `new_device`). The test partition is not edited after generation; fix it in `scripts/gen_eval_sets.py`
  when a v2 of the set is generated.
- **Bare 10–11 digit numbers are redacted as phone numbers.** `redact.py` treats any standalone run of 10 or 11
  digits as a phone number. No fact carries one today (amounts stop at 8 digits, timestamps are ISO 8601); a
  future fact with such a value would be masked before reaching a provider.
