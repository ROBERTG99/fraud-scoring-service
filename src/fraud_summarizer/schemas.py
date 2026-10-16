"""Pydantic schemas for the case summary (spec §4.6), mirrored in schemas/case_summary.v1.json."""

from __future__ import annotations

import copy
from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field

Pattern = Literal["account_takeover", "card_testing", "mule_account", "friendly_fraud", "unclear"]
PATTERNS: tuple[str, ...] = ("account_takeover", "card_testing", "mule_account", "friendly_fraud", "unclear")


class EvidenceItem(BaseModel):
    model_config = ConfigDict(extra="forbid")

    fact_key: str = Field(pattern=r"^[a-z][a-z0-9_]*(\.[a-z0-9_]+)+$", max_length=120)
    statement: str = Field(min_length=1, max_length=200)


class CaseSummaryOutput(BaseModel):
    """What the model must return for route case_summary."""

    model_config = ConfigDict(extra="forbid", title="case_summary_v1")

    pattern: Pattern
    evidence: list[EvidenceItem] = Field(min_length=1, max_length=6)
    recommended_checks: list[str] = Field(min_length=1, max_length=4)
    confidence: float = Field(ge=0.0, le=1.0)


class VerifiedEvidence(EvidenceItem):
    verified: bool


class StoredSummary(BaseModel):
    """What case_summaries.summary holds after validation and the faithfulness check."""

    model_config = ConfigDict(extra="forbid")

    pattern: Pattern
    evidence: list[VerifiedEvidence]
    recommended_checks: list[str]
    confidence: float
    model: str
    prompt_version: str
    faithfulness: float
    status: Literal["ok", "fallback", "unfaithful"]


def case_summary_json_schema() -> dict[str, Any]:
    return CaseSummaryOutput.model_json_schema()


_UNSUPPORTED = {
    "minimum",
    "maximum",
    "exclusiveMinimum",
    "exclusiveMaximum",
    "multipleOf",
    "minLength",
    "maxLength",
    "minItems",
    "maxItems",
    "pattern",
    "title",
    "default",
}
_NAME_MAPS = {"properties", "$defs", "definitions"}


def provider_schema(schema: dict[str, Any]) -> dict[str, Any]:
    """Strip constraint keywords providers' structured-output modes reject; Pydantic re-validates them locally.

    Only schema keywords are removed. Keys of `properties` / `$defs` maps are property names (a property
    can be called `pattern`) and are always kept.
    """

    def strip_schema(node: Any) -> Any:
        if isinstance(node, list):
            return [strip_schema(v) for v in node]
        if not isinstance(node, dict):
            return node
        out: dict[str, Any] = {}
        for key, value in node.items():
            if key in _NAME_MAPS and isinstance(value, dict):
                out[key] = {name: strip_schema(sub) for name, sub in value.items()}
            elif key in _UNSUPPORTED:
                continue
            else:
                out[key] = strip_schema(value)
        if out.get("type") == "object":
            out["additionalProperties"] = False
        return out

    return strip_schema(copy.deepcopy(schema))  # type: ignore[no-any-return]
