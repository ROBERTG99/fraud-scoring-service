"""Behaviour of the Claude Code hooks in .claude/hooks (spec §7)."""

from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
HOOKS = ROOT / ".claude" / "hooks"


def run_guard(payload: dict[str, object], cwd: Path = ROOT) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [sys.executable, str(HOOKS / "pre_tool_guard.py")],
        input=json.dumps({"cwd": str(cwd), "hook_event_name": "PreToolUse", **payload}),
        capture_output=True,
        text=True,
        check=False,
        env={**os.environ, "CLAUDE_PROJECT_DIR": str(cwd)},
    )


def bash(command: str) -> dict[str, object]:
    return {"tool_name": "Bash", "tool_input": {"command": command}}


@pytest.mark.parametrize(
    "command",
    [
        "git push --force origin main",
        "git push -f origin feat/x",
        "git push --force-with-lease -u origin feat/x",
        "git push origin +main",
    ],
)
def test_guard_blocks_force_push(command: str) -> None:
    result = run_guard(bash(command))
    assert result.returncode == 2, result.stderr
    assert "force" in result.stderr.lower()


@pytest.mark.parametrize(
    "command",
    [
        "terraform apply -auto-approve",
        "cd infra/terraform/envs/dev && terraform destroy",
        "git config user.name Someone",
        "git config --global user.email a@b.c",
        "export GIT_AUTHOR_NAME=x",
        "GIT_COMMITTER_EMAIL=x git commit -m y",
        'git commit -m "feat: x" -m "Co-Authored-By: Bot <b@x>"',
        "git commit -m 'feat: y\n\nClaude-Session: https://claude.ai/x'",
        "gh repo create ROBERTG99/new --public",
        "gh repo edit --visibility public",
        "cat .env",
        "rm -rf /",
        "rm -rf ~/projects",
    ],
)
def test_guard_blocks_forbidden_commands(command: str) -> None:
    result = run_guard(bash(command))
    assert result.returncode == 2, f"{command!r} was allowed"


@pytest.mark.parametrize(
    "command",
    [
        "git push -u origin feat/friendly-johnson-15rki1",
        "make check-fast",
        "terraform -chdir=infra/terraform/envs/dev validate",
        "terraform test",
        'git commit -m "feat: add scorer"',
        "cat .env.example",
        "rm -rf artifacts/model",
        "rm -rf /tmp/claude-0/gen1; grep -c '^  /v1' gen/openapi/fraud.yaml",
        "rm -rf gen/go && echo /etc",
        "git config --get user.name",
    ],
)
def test_guard_allows_normal_commands(command: str) -> None:
    result = run_guard(bash(command))
    assert result.returncode == 0, result.stderr


def test_guard_blocks_test_partition_edit(tmp_path: Path) -> None:
    part = tmp_path / "evals" / "datasets" / "decisions" / "test.jsonl"
    part.parent.mkdir(parents=True)
    part.write_text('{"id": "t1"}\n', encoding="utf-8")
    for tool in ("Edit", "Write", "MultiEdit"):
        result = run_guard({"tool_name": tool, "tool_input": {"file_path": str(part)}}, cwd=tmp_path)
        assert result.returncode == 2, tool
        assert "test partition" in result.stderr
    dev = part.with_name("dev.jsonl")
    dev.write_text("{}\n", encoding="utf-8")
    assert (
        run_guard({"tool_name": "Edit", "tool_input": {"file_path": str(dev)}}, cwd=tmp_path).returncode == 0
    )
    new = part.parent.parent / "summaries" / "test.jsonl"
    result = run_guard({"tool_name": "Write", "tool_input": {"file_path": str(new)}}, cwd=tmp_path)
    assert result.returncode == 0, "creating a new partition must be allowed"


def test_guard_blocks_shell_writes_to_test_partition(tmp_path: Path) -> None:
    part = tmp_path / "evals" / "datasets" / "decisions" / "test.jsonl"
    part.parent.mkdir(parents=True)
    part.write_text("{}\n", encoding="utf-8")
    result = run_guard(bash("echo x >> evals/datasets/decisions/test.jsonl"), cwd=tmp_path)
    assert result.returncode == 2


def test_guard_blocks_env_file_writes() -> None:
    result = run_guard({"tool_name": "Write", "tool_input": {"file_path": str(ROOT / ".env")}})
    assert result.returncode == 2
    result = run_guard({"tool_name": "Read", "tool_input": {"file_path": str(ROOT / ".env.local")}})
    assert result.returncode == 2
    result = run_guard({"tool_name": "Read", "tool_input": {"file_path": str(ROOT / ".env.example")}})
    assert result.returncode == 0


def test_guard_tolerates_malformed_input() -> None:
    result = subprocess.run(
        [sys.executable, str(HOOKS / "pre_tool_guard.py")],
        input="not json",
        capture_output=True,
        text=True,
        check=False,
    )
    assert result.returncode == 0


def test_post_edit_format_is_noop_for_unknown_files(tmp_path: Path) -> None:
    target = tmp_path / "notes.txt"
    target.write_text("  untouched  \n", encoding="utf-8")
    result = subprocess.run(
        ["bash", str(HOOKS / "post_edit_format.sh")],
        input=json.dumps({"tool_name": "Write", "tool_input": {"file_path": str(target)}}),
        capture_output=True,
        text=True,
        check=False,
    )
    assert result.returncode == 0
    assert target.read_text(encoding="utf-8") == "  untouched  \n"


def test_post_edit_format_formats_go(tmp_path: Path) -> None:
    target = tmp_path / "x.go"
    target.write_text("package x\nfunc  F( ) {}\n", encoding="utf-8")
    result = subprocess.run(
        ["bash", str(HOOKS / "post_edit_format.sh")],
        input=json.dumps({"tool_name": "Edit", "tool_input": {"file_path": str(target)}}),
        capture_output=True,
        text=True,
        check=False,
        env={**os.environ, "PATH": f"{os.environ.get('PATH', '')}:/usr/local/go/bin"},
    )
    assert result.returncode == 0
    assert "func F() {}" in target.read_text(encoding="utf-8")


def test_post_edit_format_survives_missing_tools(tmp_path: Path) -> None:
    target = tmp_path / "x.py"
    target.write_text("x=1\n", encoding="utf-8")
    result = subprocess.run(
        ["/bin/bash", str(HOOKS / "post_edit_format.sh")],
        input=json.dumps({"tool_name": "Edit", "tool_input": {"file_path": str(target)}}),
        capture_output=True,
        text=True,
        check=False,
        env={"PATH": "/nonexistent", "HOME": str(tmp_path)},
    )
    assert result.returncode == 0


def test_session_start_noop_outside_cloud() -> None:
    env = {k: v for k, v in os.environ.items() if k != "CLAUDE_CODE_REMOTE"}
    result = subprocess.run(
        ["bash", str(HOOKS / "session_start.sh")],
        input="{}",
        capture_output=True,
        text=True,
        check=False,
        env=env,
        timeout=10,
    )
    assert result.returncode == 0
    assert result.stdout.strip() == ""


def test_stop_check_honours_stop_hook_active(tmp_path: Path) -> None:
    marker = tmp_path / "ran"
    fake_make = tmp_path / "make"
    fake_make.write_text(f"#!/bin/sh\ntouch {marker}\nexit 1\n", encoding="utf-8")
    fake_make.chmod(0o755)
    env = {**os.environ, "PATH": f"{tmp_path}:{os.environ['PATH']}", "CLAUDE_PROJECT_DIR": str(ROOT)}
    result = subprocess.run(
        ["bash", str(HOOKS / "stop_check.sh")],
        input=json.dumps({"stop_hook_active": True}),
        capture_output=True,
        text=True,
        check=False,
        env=env,
    )
    assert result.returncode == 0
    assert not marker.exists()
    result = subprocess.run(
        ["bash", str(HOOKS / "stop_check.sh")],
        input=json.dumps({"stop_hook_active": False}),
        capture_output=True,
        text=True,
        check=False,
        env=env,
    )
    assert marker.exists()
    assert result.returncode == 2
    assert "check-fast" in result.stderr


def test_settings_register_all_hooks_and_keep_attribution() -> None:
    settings = json.loads((ROOT / ".claude" / "settings.json").read_text(encoding="utf-8"))
    assert settings["attribution"] == {"commit": "", "pr": "", "sessionUrl": False}
    hooks = settings["hooks"]
    for event, script in (
        ("SessionStart", "session_start.sh"),
        ("PostToolUse", "post_edit_format.sh"),
        ("PreToolUse", "pre_tool_guard.py"),
        ("Stop", "stop_check.sh"),
    ):
        assert script in json.dumps(hooks[event]), event
        assert (HOOKS / script).is_file()
    perms = settings["permissions"]
    assert "ask" not in perms
    assert "Bash(make *)" in perms["allow"]
    assert any("terraform apply" in d for d in perms["deny"])
    assert any(".env" in d for d in perms["deny"])
