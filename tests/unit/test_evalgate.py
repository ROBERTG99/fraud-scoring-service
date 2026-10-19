"""Vendored evalgate subset (D-002, D-030): suite.v1 runs, report.v1 and the check.v1 gate."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import pytest

import evalgate
from evalgate.cli import main
from evalgate.contracts import Case, Report, TargetResult
from evalgate.gate import check


def toy_target(case_input: dict[str, Any]) -> dict[str, Any]:
    """Doubles x; fails on x == 13 so the run records a target error."""
    if case_input["x"] == 13:
        raise RuntimeError("unlucky")
    return {"y": case_input["x"] * 2, "label": case_input.get("label", "a")}


@evalgate.metric("toy_even")
def toy_even(case: Case, result: TargetResult) -> float:
    assert isinstance(result.output, dict)
    return 1.0 if result.output["y"] % 4 == 0 else 0.0


def write_suite(tmp: Path, rows: list[dict[str, Any]], threshold: float = 0.5) -> Path:
    (tmp / "data.jsonl").write_text("".join(json.dumps(r) + "\n" for r in rows), encoding="utf-8")
    suite = tmp / "suite.yaml"
    suite.write_text(
        f"""schema: evalgate.suite.v1
name: toy
dataset: data.jsonl
target:
  python: test_evalgate:toy_target
plugins: [test_evalgate]
metrics:
  - name: exact_y
    kind: exact
    params: {{actual: output.y, expected: expected.y}}
    threshold: {threshold}
  - name: toy_even
    kind: custom
""",
        encoding="utf-8",
    )
    return suite


def rows(n: int, split: str = "test", wrong: int = 0) -> list[dict[str, Any]]:
    return [
        {
            "id": f"{split[0]}{i:03d}",
            "split": split,
            "input": {"x": i},
            "expected": {"y": i * 2 + (1 if i < wrong else 0)},
        }
        for i in range(n)
    ]


def test_run_scores_each_case_and_aggregates_metrics(tmp_path: Path) -> None:
    suite = write_suite(tmp_path, rows(20, wrong=5) + rows(4, split="dev"))
    out = tmp_path / "report.json"
    assert main(["run", str(suite), "--split", "test", "--out", str(out)]) == 0
    report = Report.model_validate_json(out.read_text())
    assert report.schema_ == "evalgate.report.v1" and report.suite == "toy" and report.mode == "offline"
    assert report.n_cases == 20 == len(report.cases)  # dev rows are not run on the test split
    # Case 13 raised: worst score on every metric and an error, never dropped from the report.
    c13 = next(c for c in report.cases if c.id == "t013")
    assert c13.error and "unlucky" in c13.error and c13.scores == {"exact_y": 0.0, "toy_even": 0.0}
    assert report.metrics["exact_y"].value == pytest.approx(14 / 20)  # 5 wrong expectations + 1 error
    assert report.metrics["exact_y"].passed and report.metrics["exact_y"].threshold == 0.5
    assert report.metrics["toy_even"].value == pytest.approx(10 / 20)  # even x; the error case is odd anyway
    lo, hi = report.metrics["exact_y"].ci_low, report.metrics["exact_y"].ci_high
    assert lo is not None and hi is not None and lo <= 0.7 <= hi
    assert (report.extensions or {})["target_errors"] == 1


def test_check_gates_on_thresholds_and_paired_regressions(tmp_path: Path) -> None:
    base_path, cand_path = tmp_path / "base.json", tmp_path / "cand.json"
    assert main(["run", str(write_suite(tmp_path, rows(60))), "--out", str(base_path)]) == 0
    base = Report.model_validate_json(base_path.read_text())
    # Candidate: 19 of 60 cases newly wrong (13 already errors) is far beyond 1 pp, with a CI excluding 0.
    assert main(["run", str(write_suite(tmp_path, rows(60, wrong=20))), "--out", str(cand_path)]) == 0
    cand = Report.model_validate_json(cand_path.read_text())
    result = check(cand, report_sha256="x", baseline=base, max_regression_pp=1.0)
    assert not result.passed
    reg = [f for f in result.failures if f.reason == "regression"]
    assert [f.metric for f in reg] == ["exact_y"] and reg[0].delta_pp == pytest.approx(
        -100 * 19 / 60, abs=0.01
    )
    assert check(base, report_sha256="x", baseline=base).passed  # identical runs never regress
    # Threshold: 0.99 is above what the run reaches (59/60: case 13 errors).
    strict = write_suite(tmp_path, rows(60), threshold=0.99)
    assert main(["run", str(strict), "--out", str(tmp_path / "strict.json")]) == 0
    assert main(["check", str(tmp_path / "strict.json")]) == 1
    assert main(["check", str(cand_path), "--baseline", str(base_path)]) == 1
    assert main(["check", str(base_path), "--baseline", str(base_path), "--max-regression-pp", "1.0"]) == 0


def test_check_fails_closed_without_cases_or_pairs(tmp_path: Path) -> None:
    out = tmp_path / "r.json"
    assert main(["run", str(write_suite(tmp_path, rows(5))), "--out", str(out)]) == 0
    few = Report.model_validate_json(out.read_text())
    # Five paired cases cannot support a regression claim: the gate errors instead of passing.
    assert [f.reason for f in check(few, report_sha256="x", baseline=few).failures] == ["error", "error"]
    empty = few.model_copy(update={"cases": [], "n_cases": 0})
    assert [f.metric for f in check(empty, report_sha256="x").failures] == ["n_cases"]


def test_invalid_input_exits_2(tmp_path: Path) -> None:
    suite = write_suite(tmp_path, rows(3))
    assert main(["run", str(suite), "--split", "train", "--out", str(tmp_path / "x.json")]) == 2
    assert main(["run", str(tmp_path / "missing.yaml"), "--out", str(tmp_path / "x.json")]) == 2
    (tmp_path / "bad.json").write_text('{"schema": "evalgate.report.v0"}')
    assert main(["check", str(tmp_path / "bad.json")]) == 2


def test_existing_reports_follow_the_contract() -> None:
    # evals/reports/*.json holds report.v1 files only; calibration output lives in evals/reports/calibration/.
    for path in Path("evals/reports").glob("*.json"):
        Report.model_validate_json(path.read_text())
    for path in Path("evals/reports/calibration").glob("*.json"):
        doc = json.loads(path.read_text())
        assert doc["schema"] == "evalgate.calibration.v1" and {"agreement", "passed", "n"} <= set(doc), path


def test_calibration_output_is_outside_the_report_glob() -> None:
    from evals.graders import judge
    from scripts import eval_live

    assert judge.DEFAULT_OUT.parent.name == "calibration"
    assert eval_live.CALIBRATION_OUT.parent.name == "calibration"
