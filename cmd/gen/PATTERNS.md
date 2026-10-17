# Synthetic fraud patterns (GEN_VERSION 2)

`cmd/gen` produces 100% synthetic data with seed 7. Nothing here describes a real person, merchant or
institution. The patterns are designed to be *learnable from the 13 features in `features/spec.yaml`*,
with deliberate noise and legitimate look-alikes, so the model and rules have something realistic to do.
Results on this data do not transfer to production fraud (see `docs/design-faq.md` question 15).

## Volume and time

| Size | Events | Accounts | Days | Warm-up days | Split (by day, after warm-up) |
|---|---|---|---|---|---|
| `demo` | 200,000 | 3,000 | 24 | 4 | train 14 / valid 3 / test 3 |
| `eval` | 2,000,000 | 30,000 | 24 | 4 | train 14 / valid 3 / test 3 |
| `parity` | 5,000 | 45 | 8 | 1 | parity fixture and fast training tests |

Start: 2026-09-01T00:00:00Z. Warm-up transactions carry `split=warmup` and are excluded from training and
evaluation; they only build feature history (devices, merchant categories, velocity).
Target fraud rate: 0.8% of transactions (`TestFraudRateWithinTolerance`, ± 0.05 pp).

## Legitimate behaviour

- 1 primary device bound in the first 12 h; 30% of accounts bind a second device during warm-up.
- Transactions: Poisson, about 2 per day per account, diurnal hour profile, 80% at 6–12 favourite merchants.
  Amounts are lognormal around a per-account median (about 25.00) times a category factor.
- Logins: about 0.9 per day, 97% successful.
- Look-alikes that create false positives on purpose: 4% of accounts reset their password, half of them
  bind a new phone and transact with it within an hour (triggers `R001`); 2% bind a new device; 3% travel
  abroad for 2–4 days; 5% send a large P2P transfer; 0.3% have transit tap bursts (small, frequent).
- Added in v2 (docs/DECISIONS.md D-026), so scores spread into the review band as in real queues:
  4% bind a new phone without any reset and make 1–3 purchases on it, half of them in electronics,
  department stores or jewellery at 2–8× the median (quiet-takeover twin); 2% change their email and then
  do the same (loud-takeover twin); 4% splurge on 1–3 gifts at 5–25× the median; 2.5% split a bill over
  P2P from a new phone (3–6 transfers of 20–300, mule twin); 2% make 5–12 small in-app purchases within
  minutes (card-testing twin).
- Two merchants carry prompt-injection text in their display names; they appear in ordinary purchases.

## Fraud patterns

| Pattern | Share of fraud tx | Sequence | Main signals |
|---|---|---|---|
| `account_takeover` | 37% | 0–2 failed logins from a new device, `email_changed` (60%) or `password_changed` (40%), successful login, sometimes `device_bound` (20%), then 1–4 e-commerce purchases at 8–30× the median in electronics, jewelry, digital goods or department stores | `new_device`, `minutes_since_*_change`, `amount_ratio_24h`, `mcc_novel_30d` |
| `card_testing` | 27% | 8–25 e-commerce transactions of 0.50–3.00 every 10–50 s from a new device, then (50%) one large purchase | `tx_count_10m`, small `amount_minor`, `new_device` |
| `mule_account` | 28% | 2–4 new devices bound with OTP logins, then 4–10 P2P transfers of 50–500 (round thousands) | `channel=P2P`, `distinct_devices_24h`, `amount_sum_1h_minor` |
| `friendly_fraud` | 8% | one purchase on the owner's own device at 1.5–4× the median in digital goods or marketplaces, later disputed | weak by construction (mostly undetectable) |

Low-and-slow variants (v2, share of incidents within the pattern): 50% of account takeovers are quiet (one
successful login from a new device in the home country, no credential change, 1–2 purchases at 2–5× the
median); 35% of card testing is slow (4–8 tests of 1.00–10.00 spaced 2–14 min, then 40% one purchase at
3–8× the median); 35% of mule activity uses one new device and 2–4 transfers of irregular amounts
(200–2,000) spaced 15–75 min.

Labels (`labels.csv`): every transaction of an incident is fraud; supporting login and device events are
not transactions and carry no label. `pattern=none` for legitimate transactions.

## Invariants (tested in `cmd/gen/gen_test.go`)

- Same seed and size → identical `events.jsonl` and `labels.csv` (SHA-256).
- Exactly the requested number of events; sorted by `event_ts`; strictly increasing timestamps per account.
- Every event validates against `schemas/events/*.json`.
- Train, valid and test are disjoint in time.
