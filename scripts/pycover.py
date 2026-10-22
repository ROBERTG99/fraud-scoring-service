"""Fail when line coverage of one package is below a minimum.

Usage: python scripts/pycover.py --package src/fraud_summarizer --min 85 [--data-file .coverage]
Reads the data file pytest-cov wrote; prints the measured percentage.
"""

from __future__ import annotations

import argparse
import io
import sys

import coverage


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--package", required=True)
    p.add_argument("--min", type=float, required=True)
    p.add_argument("--data-file", default=".coverage")
    args = p.parse_args(argv)
    cov = coverage.Coverage(data_file=args.data_file)
    cov.load()
    pct = cov.report(include=[f"{args.package.rstrip('/')}/*"], file=io.StringIO())
    status = "ok" if pct >= args.min else "FAIL"
    print(f"{args.package}: {pct:.1f}% line coverage (min {args.min:g}%) {status}")
    return 0 if pct >= args.min else 1


if __name__ == "__main__":
    sys.exit(main())
