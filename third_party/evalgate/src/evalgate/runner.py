"""Load a suite.v1 YAML and its JSONL dataset, run the python target on each case, build a report.v1."""

from __future__ import annotations

import asyncio
import hashlib
import importlib
import inspect
import json
import os
import time
from collections.abc import Sequence
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path
from typing import Any, Literal

import numpy as np
import yaml
from pydantic import ValidationError

from evalgate.contracts import Case, CaseReport, MetricSummary, Report, Suite, TargetResult
from evalgate.errors import InvalidInputError
from evalgate.metrics import Scorer, worst
from evalgate.stats import mean_ci


@dataclass
class LoadedSuite:
    suite: Suite
    path: Path
    dataset_path: Path
    cases: list[Case]


def load_dataset(path: Path, splits: Sequence[str]) -> list[Case]:
    if not path.is_file():
        raise InvalidInputError(f"{path}: dataset file not found")
    cases: list[Case] = []
    seen: set[str] = set()
    for n, line in enumerate(path.read_text(encoding="utf-8").splitlines(), start=1):
        if not line.strip():
            continue
        try:
            case = Case.model_validate_json(line)
        except ValidationError as exc:
            raise InvalidInputError(f"{path}:{n}: {exc.errors()[0]['msg']}") from exc
        if case.id in seen:
            raise InvalidInputError(f"{path}:{n}: duplicate case id {case.id!r}")
        seen.add(case.id)
        if case.split in splits:
            cases.append(case)
    return cases


def load_suite(path: str | Path, split: str | None = None) -> LoadedSuite:
    suite_path = Path(path)
    if not suite_path.is_file():
        raise InvalidInputError(f"{suite_path}: suite file not found")
    try:
        suite = Suite.model_validate(yaml.safe_load(suite_path.read_text(encoding="utf-8")))
    except (ValidationError, yaml.YAMLError) as exc:
        raise InvalidInputError(f"{suite_path}: {exc}") from exc
    dataset_path = (suite_path.parent / suite.dataset).resolve()
    wanted = [split] if split else list(suite.splits)
    cases = load_dataset(dataset_path, wanted)
    if not cases:
        raise InvalidInputError(f"{dataset_path}: no cases for split {wanted!r}")
    return LoadedSuite(suite, suite_path, dataset_path, cases)


def _import(ref: str) -> Any:
    module, _, attr = ref.partition(":")
    try:
        return getattr(importlib.import_module(module), attr)
    except (ImportError, AttributeError) as exc:
        raise InvalidInputError(f"cannot import {ref!r}: {exc}") from exc


def _coerce(value: Any) -> TargetResult:
    if isinstance(value, TargetResult):
        return value
    if isinstance(value, str):
        return TargetResult(output=value)
    if isinstance(value, dict):
        return TargetResult(output=value, steps=list(value.get("steps") or []))
    raise TypeError(f"target returned {type(value).__name__}; expected TargetResult, dict or str")


def _git_sha() -> str | None:
    """The checked-out commit, read from ``GITHUB_SHA`` or ``.git`` (no subprocess)."""
    if sha := os.environ.get("GITHUB_SHA"):
        return sha
    for d in (Path.cwd(), *Path.cwd().parents):
        git = d / ".git"
        if not git.is_dir():
            continue
        head = (git / "HEAD").read_text(encoding="utf-8").strip()
        if not head.startswith("ref: "):
            return head or None
        ref = head[5:]
        if (git / ref).is_file():
            return (git / ref).read_text(encoding="utf-8").strip() or None
        packed = git / "packed-refs"
        if packed.is_file():
            for line in packed.read_text(encoding="utf-8").splitlines():
                if line.endswith(" " + ref):
                    return line.split(" ", 1)[0]
        return None
    return None


async def _run_cases(loaded: LoadedSuite) -> list[tuple[Case, TargetResult | None, str | None, float]]:
    fn = _import(loaded.suite.target.python)
    is_async = inspect.iscoroutinefunction(fn)
    sem = asyncio.Semaphore(loaded.suite.concurrency)

    async def one(case: Case) -> tuple[Case, TargetResult | None, str | None, float]:
        async with sem:
            t0 = time.perf_counter()
            try:
                raw = await fn(case.input) if is_async else await asyncio.to_thread(fn, case.input)
                result: TargetResult | None = _coerce(raw)
                error = None
            except Exception as exc:  # a failing case is scored worst and reported, never dropped
                result, error = None, f"{type(exc).__name__}: {exc}"[:500]
            return case, result, error, (time.perf_counter() - t0) * 1000

    return await asyncio.gather(*(one(c) for c in loaded.cases))


def run_suite(loaded: LoadedSuite, split: str, mode: Literal["offline", "live"] = "offline") -> Report:
    suite = loaded.suite
    for plugin in suite.plugins:
        importlib.import_module(plugin)
    scorers = [Scorer(m) for m in suite.metrics]
    results = asyncio.run(_run_cases(loaded))

    case_reports: list[CaseReport] = []
    per_metric: dict[str, list[float]] = {m.name: [] for m in suite.metrics}
    metric_errors: dict[str, int] = {}
    models: set[str] = set()
    for case, result, error, measured_ms in sorted(results, key=lambda r: r[0].id):
        scores: dict[str, float] = {}
        passed = error is None
        for sc in scorers:
            if result is None:
                value = worst(sc.spec)
            else:
                try:
                    ms = sc.score(case, result)
                    value, passed = ms.value, passed and ms.passed
                except Exception:  # a broken metric fails the gate closed (metric_errors), never scores best
                    metric_errors[sc.spec.name] = metric_errors.get(sc.spec.name, 0) + 1
                    value, passed = worst(sc.spec), False
            scores[sc.spec.name] = value
            per_metric[sc.spec.name].append(value)
        if result is not None:
            models.update(str(s["model"]) for s in result.steps if s.get("model"))
        latency = result.latency_ms if result is not None and result.latency_ms > 0 else measured_ms
        cost = result.cost_usd if result is not None else 0.0
        case_reports.append(
            CaseReport(
                id=case.id,
                split=case.split,
                scores=scores,
                passed=passed,
                cost_usd=cost,
                latency_ms=latency,
                error=error,
            )
        )

    summaries: dict[str, MetricSummary] = {}
    for spec in suite.metrics:
        ci = mean_ci(per_metric[spec.name])
        ok = True
        if spec.threshold is not None:
            ok = (
                ci.estimate >= spec.threshold if spec.direction == "higher" else ci.estimate <= spec.threshold
            )
        summaries[spec.name] = MetricSummary(
            value=ci.estimate,
            threshold=spec.threshold,
            direction=spec.direction,
            passed=ok,
            ci_low=ci.low,
            ci_high=ci.high,
        )

    n = len(case_reports)
    latencies = np.asarray([c.latency_ms for c in case_reports], dtype=np.float64)
    cost_total = round(sum(c.cost_usd for c in case_reports), 6)
    target_errors = sum(1 for c in case_reports if c.error)
    now = datetime.now(UTC)
    return Report(
        schema="evalgate.report.v1",
        suite=suite.name,
        run_id=now.strftime("%Y%m%dT%H%M%SZ"),
        created_at=now.isoformat(),
        git_sha=_git_sha(),
        mode=mode,
        models=sorted(models),
        n_cases=n,
        metrics=summaries,
        cost_usd_total=cost_total,
        cost_usd_per_case=round(cost_total / n, 8) if n else 0.0,
        latency_p50_ms=float(np.percentile(latencies, 50)) if n else 0.0,
        latency_p95_ms=float(np.percentile(latencies, 95)) if n else 0.0,
        cases=case_reports,
        extensions={
            "split": split,
            "dataset": str(loaded.dataset_path.name),
            "dataset_sha256": hashlib.sha256(loaded.dataset_path.read_bytes()).hexdigest(),
            "gates": {m.name: m.gate for m in suite.metrics},
            "budget": suite.budget.model_dump() if suite.budget else {},
            "target_errors": target_errors,
            "metric_errors": metric_errors,
            "run_health": "degraded" if n and target_errors / n > 0.5 else "ok",
            "error_rate": target_errors / n if n else 0.0,
        },
    )


def write_report(report: Report, out: Path) -> Path:
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(
        json.dumps(report.model_dump(mode="json", by_alias=True), indent=2) + "\n", encoding="utf-8"
    )
    return out
