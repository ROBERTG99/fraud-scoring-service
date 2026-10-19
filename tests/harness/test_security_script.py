"""scripts/security.sh fails closed (M6 review B2, audit L5): a scanner that cannot run is not a pass.

The script runs from a copy in a temporary tree with stub tools first on PATH, so no real scanner runs.
"""

from __future__ import annotations

import os
import shutil
import stat
import subprocess
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
TOOLS = ("gitleaks", "gosec", "govulncheck", "bandit", "pip-audit", "pnpm", "hadolint", "checkov", "trivy", "tflint", "actionlint")


def _stub(bin_dir: Path, name: str, body: str) -> None:
    path = bin_dir / name
    path.write_text(f"#!/bin/sh\n{body}\n", encoding="utf-8")
    path.chmod(path.stat().st_mode | stat.S_IXUSR)


def _run(tmp_path: Path, *, uv: str, docker: str) -> subprocess.CompletedProcess[str]:
    tree = tmp_path / "repo"
    (tree / "scripts").mkdir(parents=True)
    shutil.copy(ROOT / "scripts" / "security.sh", tree / "scripts" / "security.sh")
    (tree / ".venv" / "bin").mkdir(parents=True)
    _stub(tree / ".venv" / "bin", "python", "exit 0")  # the licence check
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    for tool in TOOLS:
        _stub(bin_dir, tool, "exit 0")
    _stub(bin_dir, "uv", uv)
    _stub(bin_dir, "docker", docker)
    env = {**os.environ, "PATH": f"{bin_dir}:/usr/bin:/bin", "HOME": str(tmp_path)}
    return subprocess.run(
        ["bash", str(tree / "scripts" / "security.sh")],
        env=env,
        capture_output=True,
        text=True,
        check=False,
        timeout=60,
    )


def test_all_scanners_passing_is_a_pass(tmp_path: Path) -> None:
    out = _run(tmp_path, uv='echo "httpx==0.28.1"', docker='echo "fraud/scorer:dev"')
    assert out.returncode == 0, out.stdout
    assert "security: PASS" in out.stdout


@pytest.mark.parametrize(
    ("uv", "docker", "row"),
    [
        ("exit 1", 'echo "fraud/scorer:dev"', "pip-audit"),  # uv export fails
        ('echo ""', 'echo "fraud/scorer:dev"', "pip-audit"),  # no package listed
        ('echo "httpx==0.28.1"', "exit 1", "trivy"),  # docker compose config fails
        ('echo "httpx==0.28.1"', 'echo "grafana/grafana:12.4.12"', "trivy"),  # no fraud/ image
    ],
)
def test_a_scanner_that_cannot_run_fails_the_gate(tmp_path: Path, uv: str, docker: str, row: str) -> None:
    out = _run(tmp_path, uv=uv, docker=docker)
    assert out.returncode == 1, out.stdout
    assert any(line.startswith(row) and "NOT RUN" in line for line in out.stdout.splitlines()), out.stdout
    assert "security: FAIL" in out.stdout
