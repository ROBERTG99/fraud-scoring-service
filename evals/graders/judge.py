"""Judge calibration: agreement of the usefulness judge with labels known by construction.

`python -m evals.graders.judge --split test` measures the judge of `config/llm_routes.yaml` route
`judge_usefulness` (FakeLLM offline, the configured model with `FRAUD_JUDGE_PROVIDER` set by
`make eval-live`) on `evals/datasets/judge_calibration`. A live judge score counts only when agreement on
the test split is at least 0.8. The output follows upstream's `evalgate.calibration.v1` fields that apply to
a binary pointwise judge (D-036).
"""

from __future__ import annotations

import argparse
import asyncio
import hashlib
import json
import os
from collections import Counter
from pathlib import Path
from typing import Any

from fraud_summarizer.judge import JUDGE_PROMPT_VERSION, judge, load_judge_prompt
from fraud_summarizer.llm import LLMGateway
from fraud_summarizer.llm.factory import build_gateway
from fraud_summarizer.llm.routes import load

ROOT = Path(__file__).resolve().parents[2]
LABELS = ROOT / "evals" / "datasets" / "judge_calibration"
AGREEMENT_MIN = 0.8
# Outside evals/reports/*.json, which holds report.v1 files only.
DEFAULT_OUT = ROOT / "evals" / "reports" / "calibration" / "judge.json"


def cohen_kappa(labels: list[str], preds: list[str]) -> float:
    n = len(labels)
    if n == 0:
        return 0.0
    po = sum(a == b for a, b in zip(labels, preds, strict=True)) / n
    cl, cp = Counter(labels), Counter(preds)
    pe = sum((cl[k] / n) * (cp[k] / n) for k in set(cl) | set(cp))
    if pe >= 1.0:
        return 1.0 if po == 1.0 else 0.0
    return (po - pe) / (1 - pe)


async def _run(rows: list[dict[str, Any]], gateway: LLMGateway) -> dict[str, Any]:
    labels, preds, disagreements, errors, cost = [], [], [], 0, 0.0
    models: set[str] = set()
    for row in rows:
        label = "1" if row["expected"]["useful"] else "0"
        try:
            verdict, result = await judge(gateway, row["input"]["facts"], row["input"]["summary"])
            pred = "1" if verdict.useful else "0"
            cost += result.cost_usd
            models.add(result.model)
        except Exception:  # a failed judge call counts as a disagreement and fails the calibration
            errors += 1
            pred = "error"
        labels.append(label)
        preds.append(pred)
        if pred != label:
            disagreements.append(
                {"id": row["id"], "label": label, "pred": pred, "construction": row["tags"][-1]}
            )
    n = len(rows)
    agreement = sum(a == b for a, b in zip(labels, preds, strict=True)) / n if n else 0.0
    cats = sorted(set(labels) | set(preds))
    matrix = {a: dict.fromkeys(cats, 0) for a in cats}
    for a, b in zip(labels, preds, strict=True):
        matrix[a][b] += 1
    return {
        "n": n,
        "agreement": round(agreement, 6),
        "kappa": round(cohen_kappa(labels, preds), 6),
        "confusion": matrix,
        "judge_errors": errors,
        "disagreements": disagreements[:50],
        "models": sorted(models),
        "cost_usd": round(cost, 6),
    }


def calibrate_rows(
    rows: list[dict[str, Any]], gateway: LLMGateway, *, split: str, mode: str
) -> dict[str, Any]:
    stats = asyncio.run(_run([r for r in rows if r["split"] == split], gateway))
    checks = {"agreement": stats["agreement"] >= AGREEMENT_MIN, "judge_errors": stats["judge_errors"] == 0}
    return {
        "schema": "evalgate.calibration.v1",
        "rubric": "judge_usefulness.v1",
        "prompt_version": JUDGE_PROMPT_VERSION,
        "prompt_sha256": hashlib.sha256(load_judge_prompt().encode("utf-8")).hexdigest(),
        "labels": "evals/datasets/judge_calibration/cases.v1.jsonl",
        "split": split,
        "mode": mode,
        "thresholds": {"agreement_min": AGREEMENT_MIN},
        **stats,
        "checks": checks,
        "passed": all(checks.values()),
    }


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Calibrate the summary-usefulness judge")
    parser.add_argument("--split", choices=["dev", "test"], default="test")
    parser.add_argument("--out", type=Path, default=DEFAULT_OUT)
    args = parser.parse_args(argv)
    env = dict(os.environ)
    env.setdefault("FRAUD_JUDGE_PROVIDER", "fake")
    mode = "offline" if env["FRAUD_JUDGE_PROVIDER"] == "fake" else "live"
    rows = [json.loads(line) for line in (LABELS / "cases.v1.jsonl").read_text(encoding="utf-8").splitlines()]
    result = calibrate_rows(rows, build_gateway(load(env=env)), split=args.split, mode=mode)
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")
    verdict = "PASS" if result["passed"] else "FAIL"
    print(
        f"judge calibration ({mode}, {', '.join(result['models']) or 'no model'}) on {result['n']} {args.split} "
        f"labels: agreement {result['agreement']:.3f} (min {AGREEMENT_MIN}), kappa {result['kappa']:.3f}: {verdict}"
    )
    return 0 if result["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
