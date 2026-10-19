"""Model eval report (T-37): operating points, shards and the evalgate.report.v1 it writes."""

from __future__ import annotations

import numpy as np

from evalgate.contracts import Report
from training import evaluate as ev
from training.metrics import recall_at_fpr, recall_at_fpr_with_flags


def known_curve() -> tuple[np.ndarray, np.ndarray]:
    """1,000 negatives at 0.000..0.999 and 100 positives: 50 at 0.9995, 50 at 0.5."""
    neg = np.arange(1000) / 1000
    pos = np.r_[np.full(50, 0.9995), np.full(50, 0.5)]
    y = np.r_[np.zeros(1000, dtype=np.int8), np.ones(100, dtype=np.int8)]
    return y, np.r_[neg, pos]


def test_recall_at_fpr_on_known_curve() -> None:
    y, s = known_curve()
    recall, fpr, threshold = recall_at_fpr(y, s, 0.01)
    # 10 negatives (0.990..0.999) may pass at 1% FPR; the 50 positives at 0.9995 are above the threshold.
    assert recall == 0.5 and fpr == 0.01 and 0.989 < threshold <= 0.990
    # At 5% FPR the threshold drops to just above 0.949: still only the 50 high positives.
    assert recall_at_fpr(y, s, 0.05)[0] == 0.5
    # At 51% FPR it falls below 0.5 and every positive is caught.
    assert recall_at_fpr(y, s, 0.51)[0] == 1.0


def test_rule_flags_spend_the_fpr_budget_first() -> None:
    y, s = known_curve()
    flags = np.zeros(len(y), dtype=bool)
    flags[1050:1100] = True  # a rule catches the 50 low-scoring positives ...
    flags[:5] = True  # ... and flags 5 negatives
    recall, fpr, _ = recall_at_fpr_with_flags(y, s, flags, 0.01)
    assert recall == 1.0 and fpr == 0.01  # 5 rule false positives + 5 from the score


def test_shards_are_stable_and_balanced() -> None:
    accounts = np.asarray([f"acc_{i:08d}" for i in range(4000)])
    shards = ev.shard_of(accounts)
    assert np.array_equal(shards, ev.shard_of(accounts))
    counts = np.bincount(shards, minlength=ev.N_SHARDS)
    assert len(counts) == ev.N_SHARDS and counts.min() > 4000 / ev.N_SHARDS * 0.7


def test_report_has_global_metrics_and_paired_shard_cases() -> None:
    rng = np.random.default_rng(7)
    n = 20_000
    y = (rng.random(n) < 0.02).astype(np.int8)
    p = np.clip(0.02 + 0.6 * y + rng.normal(0, 0.1, n), 0, 1)
    flags = (y == 1) & (rng.random(n) < 0.3)
    accounts = np.asarray([f"acc_{i % 3000:08d}" for i in range(n)])
    data = ev.EvalData(y=y, p=p, flags=flags, accounts=accounts)
    report = ev.build_report(
        data, valid=data, thresholds=ev.load_thresholds(), model_version="m/1", extensions={}
    )
    Report.model_validate(report.model_dump(mode="json", by_alias=True))
    assert report.suite == "model" and report.n_cases == ev.N_SHARDS == len(report.cases)
    m = report.metrics
    assert m["recall_at_1pct_fpr_combined"].threshold == 0.80
    assert m["ece"].direction == "lower" and m["ece"].threshold == 0.02
    # The combined operating point stays inside the FPR budget (rule false positives spend it first).
    assert report.extensions["operating_point_from_valid"]["test_fpr"] <= 0.011
    assert report.extensions["gates"]["recall_at_1pct_fpr_rules"] is False
    for case in report.cases:
        assert set(case.scores) == set(m) and all(0.0 <= v <= 1.0 for v in case.scores.values())


def test_stale_eval_data_is_refused(tmp_path) -> None:  # type: ignore[no-untyped-def]
    (tmp_path / "manifest.json").write_text('{"gen_version": 1}', encoding="utf-8")
    import pytest

    with pytest.raises(ValueError, match="gen_version 1"):
        ev.check_data(tmp_path)
    assert ev.go_gen_version() == ev.GEN_VERSION


def synthetic(seed: int, n: int = 40_000, calibrated: bool = True) -> ev.EvalData:
    """Scores drawn so that P(fraud | p) = p when calibrated; p**3 otherwise (over-confident low, fine ranking)."""
    rng = np.random.default_rng(seed)
    # Mostly near 0, with 4% spread over [0.2, 1] so the review and decline bands are populated.
    tail = rng.random(n) < 0.04
    p = np.where(tail, rng.uniform(0.2, 1.0, n), rng.beta(0.3, 30.0, n))
    y = (rng.random(n) < p).astype(np.int8)
    flags = (y == 1) & (rng.random(n) < 0.2)
    accounts = np.asarray([f"acc_{i % 3000:08d}" for i in range(n)])
    return ev.EvalData(y=y, p=p if calibrated else p**3, flags=flags, accounts=accounts)


def test_active_thresholds_follow_the_scorer() -> None:
    bands = ev.active_bands()
    assert bands.version == "2026.10.2" and bands.review == 0.30 and bands.decline == 0.85


def test_gated_combined_recall_uses_the_valid_threshold() -> None:
    valid, test = synthetic(1), synthetic(2)
    report = ev.build_report(
        test, valid=valid, thresholds=ev.load_thresholds(), model_version="m/1", extensions={}
    )
    op = report.extensions["operating_point_from_valid"]
    m = report.metrics
    assert m["recall_at_1pct_fpr_combined"].value == op["test_recall"]
    assert m["fpr_combined"].value == op["test_fpr"]
    # The test-oracle point (threshold fit on test) is reported, not gated.
    oracle = recall_at_fpr_with_flags(test.y, test.p, test.flags, 0.01)[0]
    assert m["recall_at_1pct_fpr_combined_test_oracle"].value == oracle
    assert report.extensions["gates"]["recall_at_1pct_fpr_combined_test_oracle"] is False


def test_deployed_band_metrics_are_reported_and_regression_gated() -> None:
    d = synthetic(3)
    report = ev.build_report(d, valid=d, thresholds=ev.load_thresholds(), model_version="m/1", extensions={})
    m, gates = report.metrics, report.extensions["gates"]
    y = d.y.astype(bool)
    flagged = d.flags | (d.p >= 0.30)
    assert m["recall_review_band"].value == float(flagged[y].mean()) and gates["recall_review_band"] is True
    assert m["fpr_review_band"].value == float(flagged[~y].mean())
    assert m["recall_decline_band"].value == float((d.p >= 0.85)[y].mean())
    assert report.extensions["bands"] == {"rules_version": "2026.10.2", "review": 0.30, "decline": 0.85}


def test_band_calibration_gap_catches_what_ece_misses() -> None:
    good = synthetic(4)
    assert ev.band_calibration_gap(good.y, good.p, 0.30, 0.85) < 0.05
    bad = synthetic(4, calibrated=False)
    assert ev.band_calibration_gap(bad.y, bad.p, 0.30, 0.85) > 0.05
    # A constant predictor has empty bands: the gap is 1, while its ECE is under the 0.02 threshold.
    zeros = np.zeros(len(good.y))
    assert ev.band_calibration_gap(good.y, zeros, 0.30, 0.85) == 1.0
    report = ev.build_report(
        ev.EvalData(y=good.y, p=zeros, flags=good.flags, accounts=good.accounts),
        valid=good,
        thresholds=ev.load_thresholds(),
        model_version="m/1",
        extensions={},
    )
    assert not report.metrics["band_calibration_gap"].passed


def test_shards_carry_the_pooled_band_calibration_gap() -> None:
    # ~30 transactions per band per shard make a per-shard gap noise; the paired check compares the pooled value.
    d = synthetic(5)
    report = ev.build_report(d, valid=d, thresholds=ev.load_thresholds(), model_version="m/1", extensions={})
    pooled = report.metrics["band_calibration_gap"].value
    assert all(c.scores["band_calibration_gap"] == pooled for c in report.cases)
