# Synthetic fraud patterns (GEN_VERSION 1)

`cmd/gen` produces 100% synthetic data with seed 7. Nothing here describes a real person, merchant or
institution. The patterns are designed to be *learnable from the 13 features in `features/spec.yaml`*,
with deliberate noise and legitimate look-alikes, so the model and rules have something realistic to do.
Results on this data do not transfer to production fraud (see `docs/design-faq.md` question 15).

## Volume and time

| Size | Events | Accounts | Days | Warm-up days | Split (by day, after warm-up) |
|---|---|---|---|---|---|
| `demo` | 200,000 | 3,000 | 24 | 4 | train 14 / valid 3 / test 3 |
| `eval` | 2,000,000 | 30,000 | 24 | 4 | train 14 / valid 3 / test 3 |
| `parity` | 5,000 | 60 | 6 | 1 | (feature parity fixture only) |

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
- Two merchants carry prompt-injection text in their display names; they appear in ordinary purchases.

## Fraud patterns

| Pattern | Share of fraud tx | Sequence | Main signals |
|---|---|---|---|
| `account_takeover` | 37% | 0–2 failed logins from a new device, `email_changed` (60%) or `password_changed` (40%), successful login, sometimes `device_bound` (20%), then 1–4 e-commerce purchases at 8–30× the median in electronics, jewelry, digital goods or department stores | `new_device`, `minutes_since_*_change`, `amount_ratio_24h`, `mcc_novel_30d` |
| `card_testing` | 27% | 8–25 e-commerce transactions of 0.50–3.00 every 10–50 s from a new device, then (50%) one large purchase | `tx_count_10m`, small `amount_minor`, `new_device` |
| `mule_account` | 28% | 2–4 new devices bound with OTP logins, then 4–10 P2P transfers of 50–500 (round thousands) | `channel=P2P`, `distinct_devices_24h`, `amount_sum_1h_minor` |
| `friendly_fraud` | 8% | one purchase on the owner's own device at 1.5–4× the median in digital goods or marketplaces, later disputed | weak by construction (mostly undetectable) |

Labels (`labels.csv`): every transaction of an incident is fraud; supporting login and device events are
not transactions and carry no label. `pattern=none` for legitimate transactions.

## Invariants (tested in `cmd/gen/gen_test.go`)

- Same seed and size → identical `events.jsonl` and `labels.csv` (SHA-256).
- Exactly the requested number of events; sorted by `event_ts`; strictly increasing timestamps per account.
- Every event validates against `schemas/events/*.json`.
- Train, valid and test are disjoint in time.
