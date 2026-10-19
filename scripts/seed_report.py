"""Report on a database seeded by scripts/seed_host.sh: decisions by action and true pattern, review cases,
degraded decisions, new_device rate, Score latency percentiles and REVIEW reason codes.

Usage: python scripts/seed_report.py [--db NAME] [--labels artifacts/data/demo/labels.csv]
(--db defaults to the name in artifacts/seed-host/db; the database is on compose.test.yaml Postgres, 8521).
"""

from __future__ import annotations

import argparse
import collections
import csv
import sys
from pathlib import Path

import psycopg

ROOT = Path(__file__).resolve().parents[1]


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--db", default=None)
    ap.add_argument("--labels", type=Path, default=ROOT / "artifacts/data/demo/labels.csv")
    ap.add_argument("--host", default="127.0.0.1:8521")
    args = ap.parse_args(argv)
    db = args.db or (ROOT / "artifacts/seed-host/db").read_text(encoding="utf-8").strip()
    with args.labels.open(encoding="utf-8", newline="") as fh:
        pattern = {r["transaction_id"]: r["pattern"] for r in csv.DictReader(fh)}
    with psycopg.connect(f"postgresql://fraud:fraud@{args.host}/{db}?sslmode=disable") as c:
        by = collections.Counter(
            (a, pattern.get(tx, "unlabelled"))
            for tx, a in c.execute("SELECT transaction_id, action FROM decisions")
        )
        total, degraded, new_dev = c.execute(
            "SELECT count(*), count(*) FILTER (WHERE degraded),"
            " count(*) FILTER (WHERE (features->>'new_device')::float = 1) FROM decisions"
        ).fetchone() or (0, 0, 0)
        open_cases = (
            c.execute("SELECT count(*) FROM review_cases WHERE status = 'open'").fetchone() or (0,)
        )[0]
        p50, p95, p99 = (
            c.execute(
                "SELECT percentile_cont(array[0.5, 0.95, 0.99]) WITHIN GROUP (ORDER BY latency_ms) FROM decisions"
            ).fetchone()
            or ([0, 0, 0],)
        )[0]
        reasons = c.execute(
            "SELECT unnest(reason_codes), count(*) FROM decisions WHERE action = 'REVIEW' GROUP BY 1 ORDER BY 2 DESC"
        ).fetchall()
    print(
        f"database {db}: {total} decisions, {open_cases} open review cases, {degraded} degraded, new_device=1 on {new_dev}"
    )
    print(f"Score latency_ms p50 {p50:.1f}, p95 {p95:.1f}, p99 {p99:.1f}")
    for action in ("APPROVE", "REVIEW", "DECLINE"):
        row = {p: n for (a, p), n in by.items() if a == action}
        print(f"{action:8} " + ", ".join(f"{p} {n}" for p, n in sorted(row.items(), key=lambda kv: -kv[1])))
    print("REVIEW reason codes: " + ", ".join(f"{r} {n}" for r, n in reasons))
    return 0


if __name__ == "__main__":
    sys.exit(main())
