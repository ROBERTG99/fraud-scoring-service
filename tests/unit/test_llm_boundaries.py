"""Appendix B: provider SDKs only in src/fraud_summarizer/llm/; the JSON Schema mirror has no drift."""

from __future__ import annotations

import json
import re
from pathlib import Path

from fraud_summarizer.schemas import case_summary_json_schema

ROOT = Path(__file__).resolve().parents[2]
SDK_IMPORT = re.compile(r"^\s*(import|from)\s+(anthropic|openai)\b", re.MULTILINE)


def test_provider_sdks_only_in_llm_package() -> None:
    offenders = []
    for path in [
        *ROOT.joinpath("src").rglob("*.py"),
        *ROOT.joinpath("training").rglob("*.py"),
        *ROOT.joinpath("serving").rglob("*.py"),
        *ROOT.joinpath("scripts").rglob("*.py"),
    ]:
        rel = path.relative_to(ROOT).as_posix()
        if rel.startswith("src/fraud_summarizer/llm/"):
            continue
        if SDK_IMPORT.search(path.read_text(encoding="utf-8")):
            offenders.append(rel)
    assert offenders == []


def test_no_llm_sdk_in_go_scoring_path() -> None:
    go_files = [p for p in ROOT.joinpath("internal").rglob("*.go")] + [
        p for p in ROOT.joinpath("cmd").rglob("*.go")
    ]
    assert go_files
    for p in go_files:
        text = p.read_text(encoding="utf-8")
        assert "anthropic-sdk-go" not in text and "openai" not in text.lower().replace("openapi", ""), p


def test_case_summary_schema_mirror_matches_pydantic() -> None:
    mirror = json.loads((ROOT / "schemas" / "case_summary.v1.json").read_text(encoding="utf-8"))
    mirror.pop("$schema")
    mirror.pop("$id")
    assert mirror == case_summary_json_schema()


def test_model_ids_have_no_date_suffix() -> None:
    text = (ROOT / "config" / "llm_routes.yaml").read_text(encoding="utf-8")
    assert not re.search(r"claude-[a-z]+-\d-\d-\d{8}", text)
