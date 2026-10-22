#!/usr/bin/env python3
"""PreToolUse guard: blocks the actions CLAUDE.md lists under "Never do" and "Commit attribution".

Exit code 2 blocks the tool call and shows stderr to the agent. Malformed input never blocks.
Standard library only, so it runs before any dependency is installed.
"""

from __future__ import annotations

import json
import os
import re
import sys
from pathlib import Path

TRAILERS = re.compile(r"Co-Authored-By|Claude-Session|Generated with Claude Code", re.IGNORECASE)

BASH_RULES: list[tuple[re.Pattern[str], str]] = [
    (
        re.compile(r"\bterraform\b[^;&|]*\b(apply|destroy)\b"),
        "terraform apply/destroy is forbidden",
    ),
    (
        re.compile(r"\bgit\b[^;&|]*\bpush\b[^;&|]*(\s--force(-with-lease)?\b|\s-f\b|\s\+[\w./-]+)"),
        "force pushes are forbidden",
    ),
    (
        re.compile(r"\bgit\b[^;&|]*\bconfig\b(?![^;&|]*--get)[^;&|]*\buser\.\w+"),
        "changing git identity (git config user.*) is forbidden",
    ),
    (
        re.compile(r"\bGIT_(AUTHOR|COMMITTER)_(NAME|EMAIL|DATE)\s*="),
        "setting GIT_AUTHOR_*/GIT_COMMITTER_* is forbidden",
    ),
    (
        re.compile(r"\bgh\s+repo\s+(create|delete|archive|rename)\b"),
        "creating or deleting GitHub repositories is forbidden",
    ),
    (re.compile(r"\bgh\s+repo\s+edit\b[^;&|]*--visibility"), "changing repository visibility is forbidden"),
    (
        re.compile(
            r"(^|[\s;&|(])(cat|less|more|head|tail|source|\.)\s+[^;&|]*\.env(\.(?!example\b)[\w.-]+)?(\s|$)"
        ),
        "reading .env files is forbidden; use .env.example",
    ),
    (
        re.compile(r"\bcdk\s+deploy\b|\bsam\s+deploy\b|\baws\s+sagemaker\s+create-training-job\b"),
        "cloud deployments are forbidden",
    ),
]

RM_RF = re.compile(
    r"\brm\s+(-[a-zA-Z]*r[a-zA-Z]*f[a-zA-Z]*|-[a-zA-Z]*f[a-zA-Z]*r[a-zA-Z]*|-r\s+-f|-f\s+-r)\s+([^;&|\n]+)"
)
TEST_PARTITION = re.compile(r"(^|/)evals/datasets/.+/test[^/]*\.jsonl$")
ENV_FILE = re.compile(r"(^|/)\.env(\.[\w.-]+)?$")


def block(reason: str) -> None:
    print(f"Blocked by pre_tool_guard: {reason}", file=sys.stderr)
    sys.exit(2)


def project_dir(payload: dict[str, object]) -> Path:
    return Path(os.environ.get("CLAUDE_PROJECT_DIR") or str(payload.get("cwd") or os.getcwd())).resolve()


def check_rm(command: str, root: Path) -> None:
    for match in RM_RF.finditer(command):
        for raw in match.group(2).split():
            target = raw.strip("'\"")
            if target in {"/", "/*", "~", "~/", "$HOME", "${HOME}", "*", ".", ".."}:
                block(f"rm -rf {target} is outside the repository")
            if target.startswith(("~", "$HOME", "${HOME}")):
                block("rm -rf outside the repository is forbidden")
            path = Path(target)
            if not path.is_absolute():
                path = root / path
            resolved = Path(os.path.normpath(path))
            allowed = (root, Path("/tmp"), Path(os.environ.get("TMPDIR", "/tmp")))
            if not any(resolved == base or base in resolved.parents for base in allowed):
                block(f"rm -rf {target} is outside the repository")


def check_bash(command: str, root: Path) -> None:
    for pattern, reason in BASH_RULES:
        if pattern.search(command):
            block(reason)
    if re.search(r"\bgit\b[^;&|]*\b(commit|tag)\b", command) and TRAILERS.search(command):
        block("commit attribution trailers are forbidden")
    if re.search(r"\bgh\s+pr\s+(create|edit)\b", command) and TRAILERS.search(command):
        block("pull request attribution is forbidden")
    check_rm(command, root)
    for match in re.finditer(r"(>>?|\btee\b(\s+-a)?|\bsed\s+-i\S*)\s+([^\s;&|]+)", command):
        target = match.group(3).strip("'\"")
        path = Path(target) if Path(target).is_absolute() else root / target
        if TEST_PARTITION.search(path.as_posix()) and path.exists():
            block(
                "editing an existing eval test partition is forbidden; regenerate with scripts/gen_eval_sets.py"
            )


def check_file_tool(tool: str, tool_input: dict[str, object], root: Path) -> None:
    raw = str(tool_input.get("file_path") or tool_input.get("notebook_path") or "")
    if not raw:
        return
    path = Path(raw) if Path(raw).is_absolute() else root / raw
    posix = path.as_posix()
    if ENV_FILE.search(posix) and not posix.endswith(".env.example"):
        block(".env files are off limits; edit .env.example instead")
    if (
        tool in {"Edit", "Write", "MultiEdit", "NotebookEdit"}
        and TEST_PARTITION.search(posix)
        and path.exists()
    ):
        block(
            "editing an existing eval test partition is forbidden; regenerate with scripts/gen_eval_sets.py"
        )


def main() -> None:
    try:
        payload = json.loads(sys.stdin.read() or "{}")
    except json.JSONDecodeError:
        return
    if not isinstance(payload, dict):
        return
    tool = str(payload.get("tool_name") or "")
    tool_input = payload.get("tool_input") or {}
    if not isinstance(tool_input, dict):
        return
    root = project_dir(payload)
    if tool == "Bash":
        check_bash(str(tool_input.get("command") or ""), root)
    elif tool in {"Edit", "Write", "MultiEdit", "NotebookEdit", "Read"}:
        check_file_tool(tool, tool_input, root)


if __name__ == "__main__":
    main()
