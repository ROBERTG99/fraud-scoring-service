"""Repository-level checks for the project files every contributor relies on."""

from __future__ import annotations

from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]


def test_bootstrap_files_exist() -> None:
    for rel in ("CHANGELOG.md", "docs/DECISIONS.md", "docs/DEVELOPMENT.md"):
        path = ROOT / rel
        assert path.is_file(), f"missing {rel}"
        assert path.read_text(encoding="utf-8").strip(), f"{rel} is empty"
    decisions = (ROOT / "docs/DECISIONS.md").read_text(encoding="utf-8")
    assert "| # | Date | Decision | Alternatives | Reason |" in decisions


def test_attribution_settings_present() -> None:
    import json

    settings = json.loads((ROOT / ".claude/settings.json").read_text(encoding="utf-8"))
    assert settings["attribution"] == {"commit": "", "pr": "", "sessionUrl": False}
    rule = (ROOT / ".claude/rules/git-commits.md").read_text(encoding="utf-8")
    assert not rule.startswith("---"), "git-commits.md must have no frontmatter"
    assert "Co-Authored-By" in rule
