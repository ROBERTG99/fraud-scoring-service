"""The Claude Code harness (spec §7) is complete and well-formed."""

from __future__ import annotations

import json
from pathlib import Path

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[2]
CLAUDE = ROOT / ".claude"

AGENTS = [
    "reviewer",
    "security-auditor",
    "test-designer",
    "eval-analyst",
    "ux-reviewer",
    "docs-editor",
    "latency-reviewer",
    "ml-reviewer",
]
SKILLS = [
    "tdd",
    "quality-gate",
    "eval-report",
    "new-adr",
    "add-migration",
    "add-endpoint",
    "add-llm-route",
    "add-eval-case",
    "demo-capture",
    "release",
    "add-rule",
    "train-and-register-model",
    "add-fraud-pattern",
]
PATH_RULES = [
    "go",
    "python",
    "frontend",
    "proto",
    "llm-code",
    "testing",
    "evals",
    "docker",
    "terraform",
    "security",
    "docs",
]


def _frontmatter(path: Path) -> dict[str, object]:
    text = path.read_text(encoding="utf-8")
    assert text.startswith("---\n"), f"{path} has no frontmatter"
    raw = text.split("---\n", 2)[1]
    data = yaml.safe_load(raw)
    assert isinstance(data, dict)
    return data


@pytest.mark.parametrize("name", AGENTS)
def test_agents_have_frontmatter(name: str) -> None:
    meta = _frontmatter(CLAUDE / "agents" / f"{name}.md")
    assert meta["name"] == name
    assert isinstance(meta.get("description"), str) and len(str(meta["description"])) > 20


def test_reviewer_is_read_only_with_verdict() -> None:
    meta = _frontmatter(CLAUDE / "agents" / "reviewer.md")
    tools = {t.strip() for t in str(meta["tools"]).split(",")}
    assert not tools & {"Edit", "Write", "NotebookEdit"}
    body = (CLAUDE / "agents" / "reviewer.md").read_text(encoding="utf-8")
    assert "Verdict: ship" in body and "Verdict: fix first" in body


@pytest.mark.parametrize("name", SKILLS)
def test_skills_have_frontmatter(name: str) -> None:
    meta = _frontmatter(CLAUDE / "skills" / name / "SKILL.md")
    assert meta["name"] == name
    assert meta.get("description")


@pytest.mark.parametrize("name", PATH_RULES)
def test_path_rules_are_scoped(name: str) -> None:
    meta = _frontmatter(CLAUDE / "rules" / f"{name}.md")
    paths = meta.get("paths")
    assert isinstance(paths, list) and paths


def test_claude_md_under_200_lines() -> None:
    lines = (ROOT / "CLAUDE.md").read_text(encoding="utf-8").splitlines()
    assert len(lines) < 200
    text = "\n".join(lines)
    for needle in (
        "make verify",
        "Co-Authored-By",
        "PROGRESS.md",
        "docs/DECISIONS.md",
        "docs/adr",
        "terraform apply",
    ):
        assert needle in text, needle


def test_agents_md_exists() -> None:
    text = (ROOT / "AGENTS.md").read_text(encoding="utf-8")
    assert "make check-fast" in text and "Co-Authored-By" in text


def test_mcp_json_declares_playwright_and_postgres() -> None:
    data = json.loads((ROOT / ".mcp.json").read_text(encoding="utf-8"))
    servers = data["mcpServers"]
    assert "playwright" in servers and "postgres" in servers
    assert "fraud_readonly" in json.dumps(servers["postgres"])
