"""`make eval-live`: the summaries suite against a real model, within LIVE_EVAL_BUDGET_USD (spec §11).

1. Calibrate the live usefulness judge on the judge calibration test split; its scores count only when
   agreement is at least 0.8 (`evals/reports/judge_calibration_live.json`).
2. Summarize the summaries test cases with the configured provider (`FRAUD_SUMMARY_PROVIDER`, default
   anthropic) and grade them like `make eval-summary`, plus judge usefulness and cost per case.
3. Write `evals/reports/summaries_live.json` (evalgate.report.v1, mode live), which `scripts/build_report.py`
   adds to `evals/REPORT.md` with its date, model and cost.

Without credentials it prints why and exits 1; nothing is written.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import sys
from datetime import UTC, datetime
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path[:0] = [str(ROOT), str(ROOT / "src")]

from evalgate.contracts import Case, CaseReport, MetricSummary, Report  # noqa: E402
from evals import live  # noqa: E402
from evals.graders import judge as calibration  # noqa: E402
from evals.graders import summaries as grade  # noqa: E402
from fraud_summarizer.judge import judge  # noqa: E402

DATASETS = ROOT / "evals" / "datasets"
THRESHOLDS = {  # spec §11: summaries and summaries-live rows
    "schema_valid": (1.0, "higher"),
    "faithfulness": (0.95, "higher"),
    "pattern_accuracy": (0.85, "higher"),
    "pii_to_provider": (0.0, "lower"),
    "judge_usefulness": (0.80, "higher"),
    "cost_per_case": (0.003, "lower"),
}


def _rows(name: str, split: str) -> list[dict]:  # type: ignore[type-arg]
    text = (DATASETS / name / "cases.v1.jsonl").read_text(encoding="utf-8")
    return [r for r in (json.loads(line) for line in text.splitlines()) if r["split"] == split]


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Live summaries eval within a USD budget")
    parser.add_argument("--out", type=Path, default=ROOT / "evals" / "reports" / "summaries_live.json")
    parser.add_argument(
        "--calibration-out", type=Path, default=ROOT / "evals" / "reports" / "judge_calibration_live.json"
    )
    parser.add_argument("--budget", type=float, default=float(os.environ.get("LIVE_EVAL_BUDGET_USD", "2")))
    parser.add_argument("--split", choices=["dev", "test"], default="test")
    parser.add_argument("--limit", type=int, default=None, help="at most this many summary cases")
    args = parser.parse_args(argv)

    provider = os.environ.get("FRAUD_SUMMARY_PROVIDER", "anthropic")
    if (var := live.missing_key(provider)) is not None or provider == "fake":
        print(
            f"eval-live not run: set {var or 'FRAUD_SUMMARY_PROVIDER to a real provider'} (no live calls made)"
        )
        return 1
    env = {
        **os.environ,
        "FRAUD_SUMMARY_PROVIDER": provider,
        "FRAUD_JUDGE_PROVIDER": os.environ.get("FRAUD_JUDGE_PROVIDER", provider),
    }
    budget = live.Budget(limit_usd=args.budget)
    gateway = live.live_gateway(env, budget)

    calib = calibration.calibrate_rows(_rows("judge_calibration", "test"), gateway, split="test", mode="live")
    args.calibration_out.parent.mkdir(parents=True, exist_ok=True)
    args.calibration_out.write_text(json.dumps(calib, indent=2) + "\n", encoding="utf-8")
    calibrated = bool(calib["passed"])

    rows = _rows("summaries", args.split)[: args.limit]
    results = live.summarize_cases(rows, gateway)
    cases: list[CaseReport] = []
    models: set[str] = set()
    for row, res in zip(rows, results, strict=True):
        case = Case.model_validate(row)
        if res.result is None:
            worst = {
                m: (1.0 if d == "lower" and m != "cost_per_case" else 0.0) for m, (_, d) in THRESHOLDS.items()
            }
            cases.append(
                CaseReport(
                    id=row["id"],
                    split=row["split"],
                    scores=worst,
                    passed=False,
                    cost_usd=0.0,
                    latency_ms=0.0,
                    error=res.error,
                )
            )
            continue
        out = res.result.output if isinstance(res.result.output, dict) else {}
        models.add(str(out.get("model")))
        useful = 0.0
        cost = res.result.cost_usd
        if not budget.exhausted:
            facts = out.get("facts") or {}
            summary = {
                k: out[k] for k in ("pattern", "evidence", "recommended_checks", "confidence") if k in out
            }
            verdict, jres = asyncio.run(judge(gateway, facts, summary)) if facts else (None, None)
            useful = float(bool(verdict and verdict.useful))
            cost += jres.cost_usd if jres else 0.0
        scores = {
            "schema_valid": grade.schema_valid(case, res.result),
            "faithfulness": grade.faithfulness(case, res.result),
            "pattern_accuracy": float(out.get("pattern") == row["expected"]["pattern"]),
            "pii_to_provider": grade.pii_to_provider(case, res.result),
            "judge_usefulness": useful,
            "cost_per_case": round(res.result.cost_usd, 6),
        }
        cases.append(
            CaseReport(
                id=row["id"], split=row["split"], scores=scores, passed=True, cost_usd=cost, latency_ms=0.0
            )
        )

    n = len(cases)
    metrics = {}
    for name, (threshold, direction) in THRESHOLDS.items():
        value = sum(c.scores[name] for c in cases) / n if n else 0.0
        ok = value >= threshold if direction == "higher" else value <= threshold
        metrics[name] = MetricSummary(value=value, threshold=threshold, direction=direction, passed=ok)  # type: ignore[arg-type]
    errors = sum(1 for c in cases if c.error)
    now = datetime.now(UTC)
    report = Report(
        schema="evalgate.report.v1",
        suite="summaries_live",
        run_id=now.strftime("%Y%m%dT%H%M%SZ"),
        created_at=now.isoformat(),
        git_sha=None,
        mode="live",
        models=sorted(models),
        n_cases=n,
        metrics=metrics,
        cost_usd_total=round(budget.spent_usd, 6),
        cost_usd_per_case=round(budget.spent_usd / n, 8) if n else 0.0,
        latency_p50_ms=0.0,
        latency_p95_ms=0.0,
        cases=cases,
        extensions={
            "split": args.split,
            "budget_usd": args.budget,
            "judge_calibrated": calibrated,
            "judge_agreement": calib["agreement"],
            "gates": {**{m: True for m in THRESHOLDS}, "judge_usefulness": calibrated},
            "target_errors": errors,
            "metric_errors": {},
            "error_rate": errors / n if n else 0.0,
            "run_health": "degraded" if n and errors / n > 0.5 else "ok",
            "budget": {"max_cost_usd": args.budget},
        },
    )
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(
        json.dumps(report.model_dump(mode="json", by_alias=True), indent=2) + "\n", encoding="utf-8"
    )
    print(
        f"eval-live: {n} cases on {', '.join(sorted(models))}, spent USD {budget.spent_usd:.4f} of {args.budget}; "
        f"judge agreement {calib['agreement']:.3f} ({'adopted' if calibrated else 'not adopted'})"
    )
    for name, m in metrics.items():
        print(f"  {name} = {m.value:.4f} ({'pass' if m.passed else 'FAIL'})")
    gated = [m for name, m in metrics.items() if report.extensions and report.extensions["gates"][name]]
    return 0 if all(m.passed for m in gated) and errors == 0 else 1


if __name__ == "__main__":
    raise SystemExit(main())
