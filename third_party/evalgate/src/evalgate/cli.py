"""``evalgate run`` and ``evalgate check`` with upstream's options and exit codes (0 ok, 1 gate failed, 2 invalid)."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from pydantic import ValidationError

from evalgate.contracts import Report
from evalgate.errors import EXIT_GATE_FAILED, EXIT_INVALID_INPUT, EXIT_OK, InvalidInputError
from evalgate.gate import check, sha256_file
from evalgate.runner import load_suite, run_suite, write_report


def _load_report(path: Path) -> Report:
    try:
        return Report.model_validate_json(path.read_text(encoding="utf-8"))
    except (OSError, ValidationError) as exc:
        raise InvalidInputError(f"{path}: not an evalgate.report.v1 file: {exc}") from exc


def _run(args: argparse.Namespace) -> int:
    if args.split not in ("dev", "test"):
        raise InvalidInputError(f"--split must be dev or test, not {args.split!r}")
    loaded = load_suite(args.suite, split=args.split)
    report = run_suite(loaded, split=args.split, mode=args.mode)
    path = write_report(report, Path(args.out))
    err = sys.stderr.write
    err(
        f"evalgate: {report.suite} [{args.split}] run {report.run_id}: {report.n_cases} cases, mode {report.mode}\n"
    )
    for name, m in report.metrics.items():
        threshold = "" if m.threshold is None else f" (threshold {m.threshold})"
        err(
            f"  {'pass' if m.passed else 'FAIL'}  {name} = {m.value:.4f} [{m.ci_low:.4f}, {m.ci_high:.4f}]{threshold}\n"
        )
    err(f"  report: {path}\n")
    return EXIT_OK


def _check(args: argparse.Namespace) -> int:
    if args.max_regression_pp < 0:
        raise InvalidInputError("--max-regression-pp must be >= 0")
    cand = _load_report(Path(args.report))
    base = _load_report(Path(args.baseline)) if args.baseline else None
    result = check(
        cand,
        report_sha256=sha256_file(args.report),
        baseline=base,
        baseline_sha256=sha256_file(args.baseline) if args.baseline else None,
        max_regression_pp=args.max_regression_pp,
    )
    payload = result.model_dump(mode="json", by_alias=True)
    if args.out_json:
        Path(args.out_json).parent.mkdir(parents=True, exist_ok=True)
        Path(args.out_json).write_text(json.dumps(payload, indent=2) + "\n", encoding="utf-8")
    print(f"evalgate check {cand.suite}: {'PASS' if result.passed else 'FAIL'}")
    for c in result.comparisons:
        print(f"  {c.metric}: {c.baseline:.4f} -> {c.candidate:.4f} ({c.delta_pp:+.2f} pp, n={c.n_paired})")
    for f in result.failures:
        print(f"  FAIL {f.metric}: {f.reason} value={f.value} threshold={f.threshold} delta_pp={f.delta_pp}")
    return EXIT_OK if result.passed else EXIT_GATE_FAILED


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="evalgate", description=__doc__)
    sub = parser.add_subparsers(dest="cmd", required=True)
    run = sub.add_parser("run", help="Run a suite against its target and write a report.")
    run.add_argument("suite")
    run.add_argument("--out", required=True)
    run.add_argument("--split", default="test")
    run.add_argument("--mode", choices=["offline", "live"], default="offline")
    run.set_defaults(fn=_run)
    chk = sub.add_parser("check", help="Gate a report on thresholds, budgets and paired regressions.")
    chk.add_argument("report")
    chk.add_argument("--baseline")
    chk.add_argument("--max-regression-pp", type=float, default=1.0)
    chk.add_argument("--out-json")
    chk.set_defaults(fn=_check)
    args = parser.parse_args(argv)
    try:
        code = int(args.fn(args))
    except InvalidInputError as exc:
        print(f"evalgate: invalid input: {exc}", file=sys.stderr)
        return EXIT_INVALID_INPUT
    return code


if __name__ == "__main__":
    raise SystemExit(main())
