"""The Makefile declares every target documented in CLAUDE.md's "Make targets" section."""

from __future__ import annotations

import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]


def documented_targets() -> set[str]:
    text = (ROOT / "CLAUDE.md").read_text(encoding="utf-8")
    section = text.split("## Make targets")[1].split("\n## ")[0]
    targets: set[str] = set()
    for chunk in re.findall(r"`make ([^`]+)`", section):
        for word in chunk.split():
            if "=" in word:
                continue
            targets.update(t for t in word.split("/") if t)
    return targets


def makefile_targets() -> set[str]:
    text = (ROOT / "Makefile").read_text(encoding="utf-8")
    found = set(re.findall(r"^([a-zA-Z0-9_-]+):", text, flags=re.MULTILINE))
    for phony in re.findall(r"^\.PHONY:(.*)$", text, flags=re.MULTILINE):
        found.update(phony.split())
    return found


def test_documented_targets_declared() -> None:
    expected = documented_targets()
    assert {"bootstrap", "verify", "proto", "tf-check", "dlq-peek", "register-model"} <= expected
    missing = sorted(expected - makefile_targets())
    assert not missing, f"Makefile is missing targets: {missing}"


def test_verify_chains_required_gates() -> None:
    text = (ROOT / "Makefile").read_text(encoding="utf-8")
    body = text.split("\nverify:")[1].split("\n\n")[0]
    for gate in ("lint", "typecheck", "security", "test", "test-docker", "e2e", "eval", "bench", "tf-check"):
        assert re.search(rf"\$\(MAKE\) {re.escape(gate)}\b", body), f"verify does not run {gate}"
