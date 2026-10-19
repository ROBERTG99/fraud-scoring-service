# ADR 0008: Decisions come from a rules-and-model combination table, not a single score

## Status

Accepted.

## Date

2026-10-18

## Context

The card platform needs one action per transaction (APPROVE, REVIEW or DECLINE) and the reasons behind it.
Some policy is explicit and not learned: a blocked merchant category must decline whatever the model says,
and a card-testing burst must be reviewed. Risk engineers change that policy without retraining a model. The
system must keep deciding when the model or Redis is unavailable (FR-8), and every decision must be
reproducible from its inputs, the rule-set version and the model version. The model produces a calibrated
probability (ADR 0005). The spec (FR-7, §4.4) fixes a threshold table that combines both.

## Decision

- **Rules** are versioned YAML files (`rules/rules.v2026.10.2.yaml`, schema `schemas/rules.v1.json`), at most
  64 per set, each with an expr-lang condition over the typed feature environment, an action and a reason code;
  a rule can be `final`. Expressions compile with only `min`, `max` and `abs` available, and a rule that errors
  at runtime is a non-match counted in `fraud_rule_errors_total` (docs/DECISIONS.md D-016). The engine loads
  the highest version, keeps the active set when a new file is invalid, reloads on file change or
  `fr:rules:changed`, and refuses a set pinned to another feature spec (`internal/rules/engine.go`, ADR 0001).
- **Model bands** come from the same file: `thresholds.review` 0.30 and `thresholds.decline` 0.85, with
  `MODEL_SCORE_ELEVATED` and `MODEL_SCORE_HIGH` as reason codes.
- **Combination** (`internal/decide/combine.go:56-134`): severity APPROVE < REVIEW < DECLINE; a matched final
  rule wins; otherwise the action is the most severe of the matched rules and the model band. When degraded,
  the band is replaced by REVIEW at or above an amount floor (20,000 minor units without the model, 5,000
  without features) and APPROVE below it. Reason codes are ordered: final rule, other rules by ID, model code,
  degradation codes. When features are unavailable the scorer does not call the model (D-013); D-012 sets the
  floor for both flags together.
- Each decision stores `rules_version` and `model_version`, and each loaded rule set is recorded in
  `rule_sets`.

## Alternatives considered

- **A single model score with thresholds.** Simplest, but no hard policy (a blocked category could be
  approved), reason codes limited to the score band, and nothing to decide with when the model is down.
- **Rules as model inputs.** The model would learn how much each rule matters, but a policy change would need
  retraining, and a hard block could still be outweighed.
- **A weighted scorecard over rules and score.** One number again, with weights to tune and no final rules.

## Consequences

Positive:

- Policy holds regardless of the score: R003 with a model score of 0.10 reviews with `CARD_TESTING_BURST`
  (`TestRuleR003WithLowScoreReviews`, FR-7); a final rule wins (`TestFinalRuleWins`); the action never falls as
  the score rises (`TestActionMonotonicInScore`, a property test).
- Degraded decisions are defined, not improvised (`TestDegradedBandByAmount`, `TestRedisDownBiasesToReview`).
- The combined policy beats either part alone on the eval data's test split: rules alone catch 0.6012 of fraud,
  rules plus model 0.9161 at the threshold chosen on valid (FPR 0.79%), and 0.8387 at the shipped review
  threshold (FPR 0.32%) (`make eval-model`, `evals/REPORT.md`).
- Evaluation is cheap: 64 rules evaluate in 14.9 µs (`go test -bench BenchmarkEval64Rules ./internal/rules`,
  `PROGRESS.md` T-19).
- Thresholds and rules change as a new rule-set file, without touching the model.

Negative:

- Rule false positives spend the false-positive budget first: at 1% FPR fit on test, rules plus model catch
  0.9232 and the model alone 0.9244 (D-033, reported only).
- The thresholds in the rule file assume the model's calibration. A retrained model needs its band
  calibration rechecked (`band_calibration_gap` 0.0081 today, `evals/REPORT.md`, D-037) before the thresholds
  are trusted.
- The shipped review threshold runs below the 1% FPR operating point (recall 0.8387 at FPR 0.32%); moving it is
  a trade-off between recall and analyst load, made as a rule-set change (`docs/KNOWN_LIMITATIONS.md`, "Data and
  model").
- Two artifacts must stay compatible with each other and with the feature spec; the scorer checks the rule
  set's `feature_spec` and the model server checks the model's (ADR 0001).
- The decisions eval suite (1.0 on every metric) checks that the engine applies the rules and table as written;
  its labels restate the rule text, so it says nothing about whether the rules are good
  (`docs/KNOWN_LIMITATIONS.md`).
- A rule that errors at runtime fails open for that rule (a non-match), counted but not alerted on.
