"""GO_PKGS in the Makefile must resolve: Dockerfile.go at the root breaks `go list ./...`."""

from __future__ import annotations

import re
import shutil
import subprocess
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]


def test_go_pkgs_resolve() -> None:
    if shutil.which("go") is None:
        pytest.fail("go toolchain not on PATH")
    text = (ROOT / "Makefile").read_text(encoding="utf-8")
    match = re.search(r"^GO_PKGS := (.+)$", text, flags=re.MULTILINE)
    assert match, "GO_PKGS not declared"
    patterns = [p for p in match.group(1).split() if (ROOT / p.split("/...")[0]).is_dir()]
    result = subprocess.run(
        ["go", "list", *patterns], cwd=ROOT, capture_output=True, text=True, check=False, env=None
    )
    assert result.returncode == 0, result.stderr
    assert "cmd/scorer" in result.stdout


def test_makefile_never_uses_dot_dot_dot_alone() -> None:
    text = (ROOT / "Makefile").read_text(encoding="utf-8")
    assert not re.search(r"\s\./\.\.\.(\s|$)", text), "use $(GO_PKGS) instead of ./..."
