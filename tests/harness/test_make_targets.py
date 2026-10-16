"""The Makefile declares every target listed in the specification's §16 tables."""

from __future__ import annotations

import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]


def _spec_text() -> str:
    for rel in ("BUILD.md", "docs/specs/000-build-spec.md"):
        path = ROOT / rel
        if path.is_file():
            return path.read_text(encoding="utf-8")
    raise AssertionError("specification not found")


def spec_targets() -> set[str]:
    section = _spec_text().split("## 16.")[1].split("## 17.")[0]
    targets: set[str] = set()
    for line in section.splitlines():
        if not line.startswith("| `make"):
            continue
        cell = re.split(r"(?<!\\)\|", line)[1]
        for chunk in re.findall(r"`make ([^`]+)`", cell):
            first = chunk.split()[0]
            targets.update(t for t in first.replace("\\", "").split("|") if t)
    return targets


def makefile_targets() -> set[str]:
    text = (ROOT / "Makefile").read_text(encoding="utf-8")
    found = set(re.findall(r"^([a-zA-Z0-9_-]+):", text, flags=re.MULTILINE))
    for phony in re.findall(r"^\.PHONY:(.*)$", text, flags=re.MULTILINE):
        found.update(phony.split())
    return found


def test_all_block_h_targets_declared() -> None:
    expected = spec_targets()
    assert {"bootstrap", "verify", "proto", "tf-check", "dlq-peek", "register-model"} <= expected
    missing = sorted(expected - makefile_targets())
    assert not missing, f"Makefile is missing targets: {missing}"


def test_verify_chains_required_gates() -> None:
    text = (ROOT / "Makefile").read_text(encoding="utf-8")
    body = text.split("\nverify:")[1].split("\n\n")[0]
    for gate in ("lint", "typecheck", "security", "test", "test-docker", "e2e", "eval", "bench", "tf-check"):
        assert re.search(rf"\$\(MAKE\) {re.escape(gate)}\b", body), f"verify does not run {gate}"
