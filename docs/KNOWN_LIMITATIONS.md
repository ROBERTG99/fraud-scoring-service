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
- **The scorer runs below the 1% FPR operating point.** At the active rule set's review threshold (0.30) the
  combined policy catches 0.8387 of test fraud at FPR 0.32%; the 1% FPR point chosen on valid catches 0.9161
  at FPR 0.79% (`make eval-model`, `evals/reports/model.json`). Lowering the review threshold trades analyst
  load for recall; it is a rule-set change (`rules/`), not a model change.
- **Fraud is easier to catch at night.** `cmd/gen` starts fraud incidents uniformly over the day
  (`generator.go`) while legitimate activity follows a daily curve, so night hours hold a much larger share
  of fraud than of legitimate transactions, and `hour_of_day_utc` is a model feature. The M5 ML review found
  that recall drops when the model is retrained without it, most for friendly fraud; no committed command
  reproduces that ablation. Real fraud timing differs.
- **The temporal test measures sampling variation, not drift.** The model is trained on the demo data and
  evaluated on the eval data's later days; both are independent draws from a generator that does not change
  over time.
- **Demo and eval data share identifiers.** Both use seed 7, so account, device and merchant ids repeat and
  some transaction ids collide (different transactions). No feature uses an id, but loading both data sets
  into one stack breaks transaction-id idempotency.
- **ONNX parity is checked on demo data only.** The training check (`training/train.py`, 1e-5 on demo valid
  and test) does not cover the eval data, where the M5 ML review saw a few rows just over 1e-5; float32
  output also ties some scores. No decision changes at the review or decline thresholds.
- **The decisions suite is a conformance check.** Its expected actions restate the rule text, so 1.0 shows
  the Go engine applies the YAML rules and combination table as written, not that the rules are good.

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

The NFR-1 laptop target (2,000 RPS, p95 < 50 ms, p99 < 90 ms) is not met in the 4-vCPU build container,
where ghz, the scorer, BentoML, Postgres, Redis and Redpanda share the CPUs: `make bench BENCH_PROFILE=laptop`
achieved 586 of 2,000 rps with p95 603.8 ms, p99 728.5 ms and 99.3% of decisions degraded (Redis reads miss
the 8 ms budget, so features fall back). The CI profile on the same machine passes (`make bench`: 300 rps,
p95 21.8 ms, 0.87% degraded). The laptop number must be measured on a machine with the load generator on
separate cores; nothing here shows it is reachable.

The CI profile's degraded share depends on how busy the host is: 0.94% on a quiet host (`make bench` at
f9ab2a5, load 0.5) but 6.28% with a concurrent `go build` (load 8.5, M5 review), where p95 was still 42.0 ms.
`cmd/benchreport` fails over 5% degraded, so run the bench on an otherwise idle machine.

## Observability

- Jaeger in the `observability` profile keeps traces in memory (100,000 traces, the all-in-one default) and
  loses them on restart. The summarizer creates one trace per `decisions` message (APPROVE ones included),
  so a long backlog evicts older scorer spans: in a host run that consumed a topic shared with earlier runs,
  the scorer's spans were evicted before the check (`scripts/obs_host.sh`, then fresh infrastructure).
- The T-41 evidence ran the profile's images on host networking against `scripts/seed_host.sh`; the same
  check against the full compose stack after `make demo` waits on T-43 and T-46.
- The dashboard's DLQ and rule-error series are absent until the first such event (labelled counters).

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
- **`pii_to_provider = 0` covers the planted formats only.** The summaries set plants seven formats in
  merchant names (`PII_FORMATS` in `scripts/gen_eval_sets.py`: an email, two phone layouts, a 10-digit
  phone, and a test card number with spaces, dashes or dots), and the redactor was extended on dev until
  all seven were removed. The grader is an exact substring match against what the provider receives. Other
  formats (names, street addresses, IBANs, non-US phone layouts) are not measured.
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

## LLM cost cap is per process

`FRAUD_LLM_DAILY_BUDGET_USD` (D-048) is counted in memory by each case-summarizer process and restarts at
zero when the process restarts, so N replicas, or a crash loop, may spend more than the limit in one UTC day.
The per-decision cap (`summary_attempts`, 3 paid attempts) is in Postgres and holds across restarts. A shared
counter (a Postgres row per day) would make the daily cap global.

## Audit log trust boundary

The audit chain is keyed (D-050), so database access alone cannot extend it with entries that verify. The
scorer holds the key, though: a compromised scorer process can append entries that verify, and the table
owner (the migration role) or a superuser can still disable the append-only triggers. Anchors written outside
the database (`scorer audit-anchor`) make removed or rewritten entries detectable up to the last anchor;
entries after it are protected only by the key.

## Redaction errs toward removing digits

A hex identifier that contains a Luhn-valid run of 13 to 19 digits loses that run to `[REDACTED_CARD]`
(D-058): 3 of the 6,488 unique strings in the eval datasets are device hashes that do
(`uv run python scripts/redact_parity.py --rev 1d1b596`). The summary still cites the device by its fact key;
only the prompt's copy of the hash is altered. Bare 9-digit numbers are not treated as SSNs unless an "SSN"
keyword precedes them. Two more over-redactions are accepted (D-061): a 10–11 digit number followed by letters
becomes a phone ("order 1234567890th"), and a list of four 4-digit numbers that happens to pass Luhn becomes a
card ("ids 1111,2222,3333,4444").

## Faithfulness drops natural phrasing

A statement must carry its fact (D-059, D-061): "The device is new." citing `feature.new_device` is dropped
because it lacks the number. FakeLLM's statements always carry it (faithfulness 1.0, `make eval-summary`); a
live model may phrase evidence without the value, which lowers faithfulness and can turn summaries
`unfaithful`. Watch the faithfulness score in `make eval-live` before relying on live summaries.
