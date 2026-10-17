"""The evaldecide line protocol fails one case, never the run, on a mismatched reply or a hung process."""

from __future__ import annotations

import subprocess
import sys

import pytest

from evals.graders import decisions as dg

ECHO_WRONG_ID = (
    "import sys,json\nfor l in sys.stdin:\n print(json.dumps({'id':'other','action':'APPROVE'}),flush=True)"
)
HANG = "import sys,time\nfor l in sys.stdin:\n time.sleep(60)"
ECHO = "import sys,json\nfor l in sys.stdin:\n r=json.loads(l); print(json.dumps({'id':r['id'],'action':'APPROVE'}),flush=True)"


def decider(monkeypatch: pytest.MonkeyPatch, script: str, timeout: float = 5.0) -> dg._Decider:
    d = dg._Decider(timeout_s=timeout)
    monkeypatch.setattr(
        d,
        "_start",
        lambda: subprocess.Popen(
            [sys.executable, "-c", script],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            text=True,
            bufsize=1,
        ),
    )
    return d


def test_reply_ids_are_matched(monkeypatch: pytest.MonkeyPatch) -> None:
    d = decider(monkeypatch, ECHO)
    assert d.decide({"x": 1})["action"] == "APPROVE"
    assert d.decide({"x": 2})["action"] == "APPROVE"


def test_a_mismatched_reply_fails_the_case_and_restarts(monkeypatch: pytest.MonkeyPatch) -> None:
    d = decider(monkeypatch, ECHO_WRONG_ID)
    with pytest.raises(RuntimeError, match="reply for"):
        d.decide({"x": 1})
    assert d._proc is None  # the next case starts a fresh process


def test_a_hung_process_times_out(monkeypatch: pytest.MonkeyPatch) -> None:
    d = decider(monkeypatch, HANG, timeout=0.5)
    with pytest.raises(RuntimeError, match="no reply"):
        d.decide({"x": 1})
    assert d._proc is None
