"""Compare the redactor with an earlier revision of it over every string in the eval datasets.

    uv run python scripts/redact_parity.py --rev ca63a13^

Reads every JSON line of evals/datasets/*/*.jsonl, collects each string value (recursively), and redacts it
with the working-tree `fraud_summarizer.redact` and with `src/fraud_summarizer/redact.py` as it was at --rev
(`git show`). Prints the value counts and every unique value on which the two disagree. Read-only.
"""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
import types
from collections.abc import Iterator
from pathlib import Path
from typing import Any

from fraud_summarizer.redact import redact_text

ROOT = Path(__file__).resolve().parents[1]


def strings(value: Any) -> Iterator[str]:
    if isinstance(value, str):
        yield value
    elif isinstance(value, dict):
        for v in value.values():
            yield from strings(v)
    elif isinstance(value, list):
        for v in value:
            yield from strings(v)


def old_redactor(rev: str) -> types.ModuleType:
    source = subprocess.run(
        ["git", "show", f"{rev}:src/fraud_summarizer/redact.py"],
        cwd=ROOT,
        capture_output=True,
        text=True,
        check=True,
    ).stdout
    module = types.ModuleType("redact_at_rev")
    sys.modules[module.__name__] = module  # dataclasses resolve annotations through sys.modules
    exec(compile(source, f"{rev}:redact.py", "exec"), module.__dict__)  # noqa: S102  # nosec B102 - this repository's own history
    return module


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--rev", required=True, help="git revision of the redactor to compare against")
    args = ap.parse_args(argv)
    old = old_redactor(args.rev)
    files = sorted((ROOT / "evals" / "datasets").glob("*/*.jsonl"))
    values = [
        s
        for f in files
        for line in f.read_text(encoding="utf-8").splitlines()
        if line
        for s in strings(json.loads(line))
    ]
    unique = sorted(set(values))
    differ = [v for v in unique if redact_text(v) != old.redact_text(v)]
    print(
        f"{len(files)} files, {len(values)} string values, {len(unique)} unique, {len(differ)} unique values differ"
    )
    for v in differ:
        print(f"  {v[:80]!r}: now {redact_text(v)[:80]!r}, at {args.rev} {old.redact_text(v)[:80]!r}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
