"""The gate rule (as upstream): thresholds, budgets, run health and paired-bootstrap regressions."""

from __future__ import annotations

import hashlib
from pathlib import Path

from evalgate.contracts import CheckFailure, CheckResult, Comparison, Report
from evalgate.stats import DEFAULT_RESAMPLES, DEFAULT_SEED, paired_delta_ci

#: Fewer paired cases than this cannot support a regression claim; the gate fails closed with ``error``.
MIN_PAIRED = 10


def sha256_file(path: str | Path) -> str:
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def gated_metrics(report: Report) -> list[str]:
    gates = (report.extensions or {}).get("gates")
    if isinstance(gates, dict):
        return [m for m in report.metrics if gates.get(m, True)]
    return list(report.metrics)


def compare_metric(
    metric: str, baseline: Report, candidate: Report, seed: int, resamples: int
) -> Comparison | None:
    base_scores = {c.id: c.scores[metric] for c in baseline.cases if metric in c.scores}
    pairs = [
        (base_scores[c.id], c.scores[metric])
        for c in sorted(candidate.cases, key=lambda c: c.id)
        if metric in c.scores and c.id in base_scores
    ]
    if not pairs:
        return None
    base_vals = [b for b, _ in pairs]
    cand_vals = [c for _, c in pairs]
    ci = paired_delta_ci(base_vals, cand_vals, seed=seed, resamples=resamples)
    return Comparison(
        metric=metric,
        baseline=sum(base_vals) / len(base_vals),
        candidate=sum(cand_vals) / len(cand_vals),
        delta_pp=ci.estimate * 100,
        ci_low_pp=ci.low * 100,
        ci_high_pp=ci.high * 100,
        n_paired=ci.n,
    )


def is_regression(comp: Comparison, direction: str, max_regression_pp: float) -> bool:
    """Worse than ``-max_regression_pp`` and the 95% CI excludes 0 on the worse side."""
    if direction == "lower":
        return bool(comp.delta_pp > max_regression_pp and comp.ci_low_pp > 0)
    return bool(comp.delta_pp < -max_regression_pp and comp.ci_high_pp < 0)


def check(
    report: Report,
    *,
    report_sha256: str,
    baseline: Report | None = None,
    baseline_sha256: str | None = None,
    max_regression_pp: float = 1.0,
    seed: int = DEFAULT_SEED,
    resamples: int = DEFAULT_RESAMPLES,
) -> CheckResult:
    failures: list[CheckFailure] = []
    comparisons: list[Comparison] = []
    ext = report.extensions or {}
    gated = gated_metrics(report)
    if report.n_cases == 0 or not report.cases:  # nothing was measured: fail closed
        failures.append(CheckFailure(metric="n_cases", reason="error", value=0.0))
    for name in gated:
        s = report.metrics[name]
        if s.threshold is not None and (
            s.value < s.threshold if s.direction == "higher" else s.value > s.threshold
        ):
            failures.append(
                CheckFailure(metric=name, reason="threshold", value=s.value, threshold=s.threshold)
            )
    errors = ext.get("metric_errors") or {}
    for name in gated:
        if int(errors.get(name, 0)) > 0:
            failures.append(CheckFailure(metric=name, reason="error", value=float(errors[name])))
    if ext.get("run_health") == "degraded":
        failures.append(
            CheckFailure(
                metric="run_health", reason="error", value=float(ext.get("error_rate", 0.0)), threshold=0.5
            )
        )
    budget = ext.get("budget") or {}
    if budget.get("max_cost_usd") is not None and report.cost_usd_total > float(budget["max_cost_usd"]):
        failures.append(
            CheckFailure(
                metric="cost_usd_total",
                reason="budget_cost",
                value=report.cost_usd_total,
                threshold=budget["max_cost_usd"],
            )
        )
    if budget.get("max_p95_ms") is not None and report.latency_p95_ms > float(budget["max_p95_ms"]):
        failures.append(
            CheckFailure(
                metric="latency_p95_ms",
                reason="budget_p95",
                value=report.latency_p95_ms,
                threshold=budget["max_p95_ms"],
            )
        )
    if baseline is not None:
        for name in report.metrics:
            comp = (
                compare_metric(name, baseline, report, seed, resamples) if name in baseline.metrics else None
            )
            if comp is None:
                if name in gated:  # a gated metric the baseline cannot speak to: fail closed
                    failures.append(
                        CheckFailure(metric=name, reason="error", value=0.0, threshold=float(MIN_PAIRED))
                    )
                continue
            comparisons.append(comp)
            if name in gated and comp.n_paired < MIN_PAIRED:
                failures.append(
                    CheckFailure(
                        metric=name, reason="error", value=float(comp.n_paired), threshold=float(MIN_PAIRED)
                    )
                )
                continue
            if name in gated and is_regression(comp, report.metrics[name].direction, max_regression_pp):
                failures.append(
                    CheckFailure(
                        metric=name,
                        reason="regression",
                        value=comp.candidate,
                        threshold=comp.baseline,
                        delta_pp=comp.delta_pp,
                        ci_low_pp=comp.ci_low_pp,
                        ci_high_pp=comp.ci_high_pp,
                    )
                )
    return CheckResult(
        report_sha256=report_sha256,
        baseline_sha256=baseline_sha256,
        max_regression_pp=max_regression_pp,
        seed=seed,
        resamples=resamples,
        passed=not failures,
        failures=failures,
        comparisons=comparisons,
    )
