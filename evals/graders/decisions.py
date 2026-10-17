"""`decisions` suite (spec §11): target and graders.

The target streams each case through `cmd/evaldecide` (the real ScorerServer, rule set and combination
table with the feature store and model stubbed by the case), one long-lived process per eval run.
"""

from __future__ import annotations

import json
import os
import subprocess
import threading
from pathlib import Path
from typing import Any

import evalgate
from evalgate.contracts import Case, TargetResult

ROOT = Path(__file__).resolve().parents[2]
BINARY = ROOT / "artifacts" / "bin" / "evaldecide"
DEGRADED_CODES = ("MODEL_UNAVAILABLE", "FEATURES_UNAVAILABLE")


class _Decider:
    """One `evaldecide` process; cases are written and read one line at a time under a lock."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._proc: subprocess.Popen[str] | None = None

    def _start(self) -> subprocess.Popen[str]:
        env = {**os.environ, "GOTOOLCHAIN": "local"}
        subprocess.run(["go", "build", "-o", str(BINARY), "./cmd/evaldecide"], cwd=ROOT, env=env, check=True)
        return subprocess.Popen(
            [str(BINARY)], cwd=ROOT, stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True, bufsize=1
        )

    def decide(self, row: dict[str, Any]) -> dict[str, Any]:
        with self._lock:
            if self._proc is None or self._proc.poll() is not None:
                self._proc = self._start()
            assert self._proc.stdin is not None
            assert self._proc.stdout is not None
            self._proc.stdin.write(json.dumps({"id": "case", "input": row}) + "\n")
            self._proc.stdin.flush()
            out: dict[str, Any] = json.loads(self._proc.stdout.readline())
        if out.get("error"):
            raise RuntimeError(out["error"])
        return out


_DECIDER = _Decider()


def target(row: dict[str, Any]) -> TargetResult:
    out = _DECIDER.decide(row)
    out.pop("id", None)
    steps = [{"node": "score", "model": None}]
    return TargetResult(output=out, steps=steps)


def _out(result: TargetResult) -> dict[str, Any]:
    return result.output if isinstance(result.output, dict) else {}


@evalgate.metric("reason_code_set")
def reason_code_set(case: Case, result: TargetResult) -> float:
    """1 when the decision's reason codes are exactly the expected set (no missing, no extra code)."""
    got = _out(result).get("reason_codes")
    want = (case.expected or {}).get("reason_codes")
    return float(isinstance(got, list) and isinstance(want, list) and set(got) == set(want))


@evalgate.metric("degraded_handling")
def degraded_handling(case: Case, result: TargetResult) -> float:
    """A degraded case carries the flag and its degraded reason code; a healthy one carries neither."""
    out, exp = _out(result), case.expected or {}
    codes = set(out.get("reason_codes") or [])
    want = {c for c in exp.get("reason_codes", []) if c in DEGRADED_CODES}
    return float(out.get("degraded") is bool(exp.get("degraded")) and codes & set(DEGRADED_CODES) == want)
