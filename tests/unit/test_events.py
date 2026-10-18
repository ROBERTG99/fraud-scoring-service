from __future__ import annotations

import json
from pathlib import Path

import pytest

from fraud_summarizer.events import InvalidEventError, validate, validate_topic

FIXTURES = Path(__file__).resolve().parents[1] / "fixtures" / "events"


def test_shared_fixtures_agree() -> None:
    """Same manifest as internal/stream/validate_test.go::TestSharedFixtures."""
    manifest = json.loads((FIXTURES / "manifest.json").read_text(encoding="utf-8"))
    assert sum(m["valid"] for m in manifest) == 20
    assert sum(not m["valid"] for m in manifest) == 22
    for entry in manifest:
        raw = (FIXTURES / entry["file"]).read_bytes()
        if entry["valid"]:
            name, _ = validate(raw)
            assert name.split(".")[-2] in {entry["kind"], "decision", "dlq"}
        else:
            with pytest.raises(InvalidEventError):
                validate(raw)


def test_topic_mismatch_rejected() -> None:
    raw = (FIXTURES / "valid" / "login_01.json").read_bytes()
    assert validate_topic("events.login", raw)["account_id"] == "acc_000123"
    with pytest.raises(InvalidEventError, match="expects schema"):
        validate_topic("events.device", raw)


@pytest.mark.parametrize("raw", [b"{not json", b"[1,2]", b'{"schema": "fraud.unknown.v1"}'])
def test_malformed_rejected(raw: bytes) -> None:
    with pytest.raises(InvalidEventError):
        validate(raw)
