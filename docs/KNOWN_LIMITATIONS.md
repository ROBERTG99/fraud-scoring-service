# Known limitations

What is not done, approximate or out of scope. Each entry names where it shows up and how it is bounded.

## Features

- **Out-of-order device and MCC sightings are approximate.** `fr:{acc}:dev` and `fr:{acc}:mcc` keep only the
  latest sighting per member (`ZADD GT`). If an event later than a transaction arrives before the
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
  of fraud than of legitimate transactions, and `hour_of_day_utc` is a model feature. An ML review found
  that recall drops when the model is retrained without it, most for friendly fraud; no committed command
  reproduces that ablation. Real fraud timing differs.
- **The temporal test measures sampling variation, not drift.** The model is trained on the demo data and
  evaluated on the eval data's later days; both are independent draws from a generator that does not change
  over time.
- **Demo and eval data share identifiers.** Both use seed 7, so account, device and merchant ids repeat and
  some transaction ids collide (different transactions). No feature uses an id, but loading both data sets
  into one stack breaks transaction-id idempotency.
- **ONNX parity is checked on demo data only.** The training check (`training/train.py`, 1e-5 on demo valid
  and test) does not cover the eval data, where a model review saw a few rows just over 1e-5; float32
  output also ties some scores. No decision changes at the review or decline thresholds.
- **The decisions suite is a conformance check.** Its expected actions restate the rule text, so 1.0 shows
  the Go engine applies the YAML rules and combination table as written, not that the rules are good.

## Serving

- **Triton was not run in the build environment.** `nvcr.io/nvidia/tritonserver` is blocked by the network
  policy (pull returned `Forbidden` on 2026-10-16). The model repository (`serving/triton/`) and KServe v2
  client (`internal/model/triton.go`, `TestKServeV2RequestShape` against a recorded request/response) exist,
  but `TestTritonGoldenVector` skips unless `FRAUD_TEST_TRITON_URL` points at a running Triton. The `triton`
  profile exists in `compose.yaml` (`make triton-up` after `scripts/triton_repo.sh`) but was not started
  in the reference environment: the image is several GB and was not pulled. `make verify` never requires Triton.

## Model server throughput on a shared host

The Python model server (BentoML) is the scorer's throughput limit when everything runs on one 4-CPU
machine. Measurements in docs/DECISIONS.md D-024: without request batching, 250 tx/s with shadow scoring
degraded 40% of decisions to rules-only (`MODEL_UNAVAILABLE`). The batching client fixed that for the seed
rate: 63 of 16,863 degraded (0.37%) on gen v1 data (D-024) and 59 of 16,904 on gen v2 data
(`scripts/seed_report.py`). Higher rates (the 1,000 RPS load target) need more model-server
workers or Triton.

The laptop target (2,000 RPS, p95 < 50 ms, p99 < 90 ms) is not met in the 4-vCPU build container,
where ghz, the scorer, BentoML, Postgres, Redis and Redpanda share the CPUs: `make bench BENCH_PROFILE=laptop`
achieved 586 of 2,000 rps with p95 603.8 ms, p99 728.5 ms and 99.3% of decisions degraded (Redis reads miss
the 8 ms budget, so features fall back). The CI profile on the same machine passes (`make bench` at f9ab2a5:
300 rps, p95 19.6 ms, 0.94% degraded), and on GitHub Actions at f1dc295 (p95 12.8 ms, 1.58% degraded;
[`evals/REPORT.md`](../evals/REPORT.md)). The laptop number must be measured on a machine with the load
generator on separate cores; nothing here shows it is reachable.

The CI profile's degraded share depends on how busy the host is: 0.94% on a quiet host (`make bench` at
f9ab2a5, load 0.5) but 6.28% with a concurrent `go build` (load 8.5), where p95 was still 42.0 ms.
`cmd/benchreport` fails over 5% degraded, so run the bench on an otherwise idle machine.

## Observability

- Jaeger in the `observability` profile keeps traces in memory (100,000 traces, the all-in-one default) and
  loses them on restart. The summarizer creates one trace per `decisions` message (APPROVE ones included),
  so a long backlog evicts older scorer spans: in a host run that consumed a topic shared with earlier runs,
  the scorer's spans were evicted before the check (`scripts/obs_host.sh`, then fresh infrastructure).
- The observability check (`scripts/obs_host.sh`) ran the profile's images on host networking against `scripts/seed_host.sh`. The same
  check against the compose stack (`docker compose --profile observability up -d`) was not run.
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

## Audit key rotation

The audit chain records no key id, so entries verify only under the key that wrote them; rotating
`FRAUD_AUDIT_KEY` needs a new chain (`docs/runbook.md`, Key rotation). A key id per entry and a list of retired
keys for verification would allow rotation in place.

## A replay after a lost idempotency value adds one feature event

When a transaction's Redis value is missing (expired, failed its MAC after a key change, or never written
because the 8 ms put failed), a replay is scored before Postgres finds the conflict, so the feature store gains
the replayed event once per replay in flight, even when the replay is then refused with ALREADY_EXISTS. The
conflict path writes the stored decision back to Redis, so later replays are answered there
(`TestAConflictRestoresTheIdempotencyEntry`, D-064). Decisions stored before request hashes existed are not
written back (Redis values need the hash) and add an event on every replay. Deduplicating feature events on the transaction_id across accounts would remove the residual.

## A merchant name may carry clearing words

Faithfulness sets one occurrence of a merchant name aside before the clearing screen (D-062, D-064): a
statement that only quotes a merchant called "Not Fraud Ltd" is kept, while any clearing word outside that
quote drops it. The console should show quoted merchant names as data (they are rendered as text, never HTML).

## Redaction gaps that remain

Card digits are found in groups joined by up to 8 characters that are not letters or digits, by percent
escapes and by HTML entities (D-064 to D-067): escapes are decoded to a fixpoint and folded as plain text is
(dashes, NFKC, format characters removed); a decoded letter keeps its escape, and any other decoded character
that is not a digit, "@" or plain punctuation becomes a space. Out of scope: separators of 9 or more characters, separators
that contain letters or digits other than those escapes, card layouts other than the common shapes and equal
groups, a card written as an IPv4 address or inside a UUID (held as identifiers), a card split by an email or
a date, a phone glued to the extension of the item before it (`x12415-555-2671`), escape schemes other than
percent escapes and HTML references (`%u0034`, `\x34`), and digits read as part of a longer numeric reference
(`&#52111` is one character, as HTML reads it, though another reader might see `&#52` and `111`). Only merchant names
(at most 120 characters) reach a provider as third-party free text.

## Merged card windows remove numbers from lists

The wide pass redacts every Luhn-valid window of groups in a card layout, equal groups included (D-065), so
a merchant name holding a list of numbers (prices such as "12.99, 45.10, ...", versions such as
"2.10.3 / 4.5.6", two 8-digit references) can lose some of them when a window passes Luhn by chance. The
eval datasets are unchanged (`uv run python scripts/redact_parity.py --rev 1b7c746`: 0 of 6,488 unique values
differ).

## The clearing screen drops some ordinary phrases

Clearing phrases match with any punctuation or none between their words (D-065, D-066), so statements such
as "3 prior non-fraud chargebacks", "safe-to-spend balance", "a no-fraud-history segment" or "checks-out
flow" are dropped. That is the safe direction: an unverified item is left out of the summary, never
added. No fact key, rule or prompt in the repository contains such a phrase, and `make eval` passes.

## Faithfulness drops natural phrasing

A statement must carry its fact (D-059, D-061): "The device is new." citing `feature.new_device` is dropped
because it lacks the number. FakeLLM's statements always carry it (faithfulness 1.0, `make eval-summary`); a
live model may phrase evidence without the value, which lowers faithfulness and can turn summaries
`unfaithful`. Watch the faithfulness score in `make eval-live` before relying on live summaries.

## The demo depends on the shipped model

`make demo` asserts that the scripted takeover is REVIEWed, which needs the model to score it in the
review band (0.30 to 0.85). The shipped model weighs the hour of day: the scripted account's features score
1.00 at hour 4 UTC and 0.65 at hour 14 (D-071), so the scenario places the purchase between 13:00 and 15:00
UTC. A retrained model can move the score out of the band; the `ato_review` checkpoint then fails and
says what came back. "Stopping" the model server means pausing it: SIGSTOP on the host stack,
`docker compose pause` on the compose stack, so the scorer sees it unreachable without a restart. The shadow
checkpoint compares the production version with itself (agreement 100%) until a distinct shadow version is
registered, and the risk engineer's promotion decision (`make register-model ALIAS=production`) is not
scripted.

## The compose stack on a laptop

Measured on Docker Desktop (macOS arm64, 10 vCPU and 8 GB for the VM) with the repository inside a synced
cloud-drive folder:

- The degraded demo checkpoint (`model_unavailable_rules_only`, every call under the 45 ms Score budget with the
  model server paused) first passed 2 of 3 runs here and failed once on Linux CI (45.9 ms): the commit took the
  time the model left and spooled after the deadline. With D-078's spool reserve, 8 of 8 consecutive runs passed
  (slowest 27.1 to 31.4 ms). An fsync slower than the 12 ms reserve still overruns the budget; on Docker Desktop
  traces showed commits up to 33.9 ms. The same overrun reaches GitHub Actions: the `e2e` workflow passed this checkpoint on
  e63e191 and 9a28857 and failed it on cb95269 (slowest 81.6 ms of 20; the scorer log shows one decision of that
  phase spooled and replayed), so the console steps that follow it did not run. A degraded decision whose commit
  misses its window waits for the spool fsync; answering before the fsync would break commit-before-respond
  (ADR 0007), so the budget holds only while the disk fsyncs within the reserve.
- `make seed` scored 16,943 transactions with 680 degraded (531 `MODEL_UNAVAILABLE`, 149
  `FEATURES_UNAVAILABLE`): the 20 ms model and 8 ms Redis budgets are tight for a 250 tx/s replay inside the
  VM. The host stack (`scripts/seed_host.sh`) degrades far fewer.
- `make test-docker` runs the Go packages four at a time (`GOFLAGS=-p=4`): with all of them at once under
  `-race`, one integration test missed its 8 ms Redis budget for lack of CPU (D-073).
- The model server's compose healthcheck is liveness (`/livez`): before `make seed` registers a model the stack
  is healthy and scores rules-only (D-073).

## The SageMaker module is validated, not exercised

`make tf-check` validates the module and asserts encryption, public access and IAM scope with a mocked
provider; nothing is applied. The execution role lacks `ecr:GetAuthorizationToken` (AWS grants it only on
`Resource: "*"`, which the module's no-wildcard test refuses), so a real training job would fail to pull its
image until a deployment adds that one statement, and `logs:CreateLogGroup` is scoped to a log-stream ARN where
it never matches (`docs/security.md`, open finding 18).

## One scorer per spool directory

The disk spool assumes one scorer process per `FRAUD_SPOOL_DIR`: `Drain` renames the active file, so a second
process sharing the directory could keep writing into a file the first one replays and deletes. Compose runs
one scorer on a named volume; replicas need a directory each.

## The trained model depends on the CPU it is trained on

XGBoost's training is reproducible on one platform (CI on Linux x86-64 rebuilds the committed baseline model
bit for bit: `make eval` there gives `band_calibration_gap` 0.0081 and Gate PASS, `evals/REPORT.md`) but not
across platforms. From the same data, seed and `n_jobs`, macOS arm64 trained `gbdt-0b1d9e9ed9` and Linux arm64
`gbdt-741d763432`, and both miss one gate of `evals/suites/model.yaml`: `band_calibration_gap` 0.0759 and 0.0920 (limit
0.05; PR-AUC 0.8617 and 0.8646, recall@1%FPR 0.9122 and 0.9168, both above their thresholds). So `make eval`,
and with it `make verify`, fails at `eval-model` on arm64 machines. The gate is not
loosened: train the model that ships on x86-64 (CI, or the SageMaker launcher's `ml.m5` instances), and treat
the decline-band calibration as model-specific until isotonic calibration on the decline band is made robust.
