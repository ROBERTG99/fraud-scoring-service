"""The documentation set (spec §18, T-47): every document exists, and every number in the README's results
comes with the command or report that produced it."""

from __future__ import annotations

import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]

DOCUMENTS = [
    "README.md",
    "docs/architecture.md",
    "docs/ai-development.md",
    "docs/evaluation.md",
    "docs/security.md",
    "docs/runbook.md",
    "docs/DEVELOPMENT.md",
    "docs/KNOWN_LIMITATIONS.md",
    "docs/verification.md",
    "docs/design-faq.md",
    "evals/REPORT.md",
    "CONTRIBUTING.md",
    "SECURITY.md",
    "CHANGELOG.md",
    "LICENSE",
]
# Spec §18's eight ADRs; ADR 0001 predates the list, so they are 0002 to 0009 here (D-070).
ADRS = ["0002", "0003", "0004", "0005", "0006", "0007", "0008", "0009"]
# A results line names its source: a Markdown link, or a `make ...` / `scripts/...` / report path in backticks.
SOURCE = re.compile(r"\]\(|`(?:make |scripts/|evals/|uv run |go test )[^`]*`")


def test_every_document_exists() -> None:
    missing = [d for d in DOCUMENTS if not (ROOT / d).is_file()]
    missing += [f"docs/adr/{n}-*.md" for n in ADRS if not list((ROOT / "docs/adr").glob(f"{n}-*.md"))]
    assert not missing, missing


def test_the_design_faq_answers_every_question_with_file_references() -> None:
    faq = (ROOT / "docs/design-faq.md").read_text()
    sections = re.split(r"^## ", faq, flags=re.M)[1:]
    assert len(sections) == 15, len(sections)
    for section in sections:
        assert re.search(r"[\w./-]+\.\w+:\d+", section), section.splitlines()[0]


def _results(readme: str) -> list[str]:
    """The Results section as table rows and paragraphs (a wrapped sentence is one item)."""
    m = re.search(r"^## Results\n(.*?)(?=^## )", readme, flags=re.M | re.S)
    assert m, "README has no Results section"
    items: list[str] = []
    paragraph: list[str] = []
    for line in [*m.group(1).splitlines(), ""]:
        if line.startswith("|") or not line.strip():
            if paragraph:
                items.append(" ".join(paragraph))
                paragraph = []
            if line.startswith("|"):
                items.append(line)
        else:
            paragraph.append(line.strip())
    return items


def test_readme_numbers_link_to_reports() -> None:
    readme = (ROOT / "README.md").read_text()
    unsourced = [
        line
        for line in _results(readme)
        if re.search(r"\d", line) and not line.startswith("|---") and not SOURCE.search(line)
    ]
    assert not unsourced, unsourced
    assert "under construction" not in readme.lower()
