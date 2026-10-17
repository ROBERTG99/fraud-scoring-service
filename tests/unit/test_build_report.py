"""evals/REPORT.md generation (T-40)."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from scripts import build_report as br


def report(
    suite: str, metrics: dict[str, tuple[float, float | None, str]], *, mode: str = "offline", **kw: Any
) -> dict[str, Any]:
    cases = [
        {
            "id": f"c{i:02d}",
            "split": "test",
            "scores": {m: v for m, (v, _, _) in metrics.items()},
            "passed": True,
            "cost_usd": 0.0,
            "latency_ms": 1.0,
            "error": None,
        }
        for i in range(12)
    ]
    return {
        "schema": "evalgate.report.v1",
        "suite": suite,
        "run_id": "20261017T120000Z",
        "created_at": "2026-10-17T12:00:00+00:00",
        "git_sha": "abc1234def",
        "mode": mode,
        "models": kw.get("models", []),
        "prompt_hashes": {},
        "n_cases": len(cases),
        "metrics": {
            m: {
                "value": v,
                "threshold": t,
                "direction": d,
                "passed": t is None or (v >= t if d == "higher" else v <= t),
                "ci_low": None,
                "ci_high": None,
            }
            for m, (v, t, d) in metrics.items()
        },
        "cost_usd_total": 0.0,
        "cost_usd_per_case": 0.0,
        "latency_p50_ms": 2.0,
        "latency_p95_ms": kw.get("p95", 4.0),
        "cases": cases,
        "extensions": {"split": "test", "gates": {m: True for m in metrics}, **kw.get("ext", {})},
    }


def write(d: Path, name: str, data: dict[str, Any]) -> None:
    d.mkdir(parents=True, exist_ok=True)
    (d / f"{name}.json").write_text(json.dumps(data), encoding="utf-8")


def test_report_states_mode(tmp_path: Path) -> None:
    reports, baseline = tmp_path / "reports", tmp_path / "reports" / "baseline"
    write(reports, "decisions", report("decisions", {"action_exact": (1.0, 1.0, "higher")}))
    summaries = report(
        "summaries",
        {"pattern_accuracy": (0.9028, 0.85, "higher"), "pii_to_provider": (0.0, 0.0, "lower")},
        models=["fake-case-summary-v1"],
    )
    write(reports, "summaries", summaries)
    write(
        baseline,
        "summaries",
        report(
            "summaries",
            {"pattern_accuracy": (0.8889, 0.85, "higher"), "pii_to_provider": (0.0, 0.0, "lower")},
        ),
    )
    text = br.render(reports, baseline)
    assert "Mode: **offline**" in text
    assert "case_summary.v1" in text and "fake-case-summary-v1" in text
    assert "| summaries | pattern_accuracy | 0.9028 | ≥ 0.85 | pass | 0.8889 | +1.39 |" in text
    assert "| decisions | action_exact | 1.0000 | ≥ 1 | pass | – | – |" in text  # no baseline yet
    assert "`make eval-summary`" in text and "`make eval-decisions`" in text
    assert "model: not run" in text and "latency: not run" in text  # missing reports are stated, not hidden
    assert "do not edit" in text


def test_live_or_cassette_mode_is_stated(tmp_path: Path) -> None:
    reports = tmp_path / "r"
    write(
        reports,
        "summaries",
        report(
            "summaries", {"pattern_accuracy": (0.9, 0.85, "higher")}, mode="live", models=["claude-haiku-5-5"]
        ),
    )
    assert "Mode: **live**" in br.render(reports, tmp_path / "none")
    write(
        reports,
        "summaries",
        report("summaries", {"pattern_accuracy": (0.9, 0.85, "higher")}, models=["claude-haiku-5-5"]),
    )
    assert "Mode: **cassette**" in br.render(reports, tmp_path / "none")


def test_gate_failures_are_listed(tmp_path: Path) -> None:
    reports = tmp_path / "r"
    write(reports, "decisions", report("decisions", {"action_exact": (0.98, 1.0, "higher")}))
    text = br.render(reports, tmp_path / "none")
    assert "| decisions | action_exact | 0.9800 | ≥ 1 | **FAIL** |" in text
    assert "Gate: **FAIL**" in text


def test_live_evals_not_run_are_stated(tmp_path: Path) -> None:
    reports = tmp_path / "r"
    write(reports, "decisions", report("decisions", {"action_exact": (1.0, 1.0, "higher")}))
    assert "Live evals (`make eval-live`): not run" in br.render(reports, tmp_path / "none")
    write(
        reports,
        "summaries_live",
        report(
            "summaries", {"judge_usefulness": (0.9, 0.8, "higher")}, mode="live", models=["claude-haiku-5-5"]
        ),
    )
    text = br.render(reports, tmp_path / "none")
    assert "Live evals (`make eval-live`): run" in text and "claude-haiku-5-5" in text


def test_judge_calibration_is_reported(tmp_path: Path) -> None:
    reports = tmp_path / "r"
    write(reports, "decisions", report("decisions", {"action_exact": (1.0, 1.0, "higher")}))
    assert "Judge calibration (`make eval-judge`): not run" in br.render(reports, tmp_path / "none")
    calib = {
        "schema": "evalgate.calibration.v1",
        "mode": "offline",
        "models": ["fake-judge-v1"],
        "split": "test",
        "n": 40,
        "agreement": 0.925,
        "kappa": 0.85,
        "thresholds": {"agreement_min": 0.8},
        "passed": True,
    }
    (reports / "calibration").mkdir()
    (reports / "calibration" / "judge.json").write_text(json.dumps(calib), encoding="utf-8")
    text = br.render(reports, tmp_path / "none")
    assert (
        "FakeLLM's classifier): offline, fake-judge-v1, 40 test labels: agreement 0.925 (min 0.8), kappa 0.850: PASS"
        in text
    )
    calib["passed"] = False
    (reports / "calibration" / "judge.json").write_text(json.dumps(calib), encoding="utf-8")
    assert "Gate: **FAIL**" in br.render(reports, tmp_path / "none")


def _all_required(reports: Path, baseline: Path | None) -> None:
    for name in ("model", "decisions", "summaries"):
        rep = report(name, {"m": (1.0, 1.0, "higher")})
        write(reports, name, rep)
        if baseline is not None:
            write(baseline, name, rep)


def test_missing_required_suite_fails_the_gate(tmp_path: Path) -> None:
    reports, baseline = tmp_path / "r", tmp_path / "b"
    _all_required(reports, baseline)
    (reports / "summaries.json").unlink()
    text = br.render(reports, baseline)
    assert "Gate: **FAIL**" in text and "summaries: not run" in text


def test_missing_baseline_fails_the_gate(tmp_path: Path) -> None:
    reports, baseline = tmp_path / "r", tmp_path / "b"
    _all_required(reports, baseline)
    (baseline / "decisions.json").unlink()
    text = br.render(reports, baseline)
    assert "Gate: **FAIL**" in text and "no baseline" in text


def test_latency_not_run_is_stated_in_the_gate_line(tmp_path: Path) -> None:
    reports, baseline = tmp_path / "r", tmp_path / "b"
    _all_required(reports, baseline)
    gate = next(line for line in br.render(reports, baseline).splitlines() if line.startswith("Gate:"))
    assert gate.startswith("Gate: **PASS**") and "latency not run" in gate
