"""Faithfulness check without an LLM (spec §4.6).

An evidence item is verified when its `fact_key` exists in the case facts and every number in its
statement equals that fact's value. Score = verified / total. Unverified items are dropped; when none
remain the summary is `unfaithful`.
"""

from __future__ import annotations

import math
import re
from dataclasses import dataclass

# Digits that continue an identifier (tx_count_10m, mcc_novel_30d, R003) are not numbers; anything else is,
# including numbers with a unit or suffix attached ("12x", "30min", "940USD") and leading-dot decimals (".5").
NUMBER = re.compile(r"(?<![A-Za-z_\d.])-?(?:(?:\d{1,3}(?:,\d{3})+|\d+)(?:\.\d+)?|\.\d+)")


@dataclass(frozen=True)
class CheckedItem:
    fact_key: str
    statement: str
    verified: bool
    reason: str


@dataclass(frozen=True)
class FaithfulnessResult:
    items: list[CheckedItem]
    score: float

    @property
    def kept(self) -> list[CheckedItem]:
        return [i for i in self.items if i.verified]


def numbers_in(text: str) -> list[float]:
    return [float(m.group(0).replace(",", "")) for m in NUMBER.finditer(text)]


def _as_number(value: str) -> float | None:
    try:
        x = float(value)
    except ValueError:
        return None
    return x if math.isfinite(x) else None


def check_item(fact_key: str, statement: str, facts: dict[str, str]) -> CheckedItem:
    if fact_key not in facts:
        return CheckedItem(fact_key, statement, False, "fact_key not in facts")
    nums = numbers_in(statement)
    if not nums:
        return CheckedItem(fact_key, statement, True, "no numbers")
    value = _as_number(facts[fact_key])
    if value is None:
        return CheckedItem(fact_key, statement, False, "statement has numbers but the fact is not numeric")
    for n in nums:
        if n != value:
            return CheckedItem(fact_key, statement, False, f"number {n:g} != fact value {value:g}")
    return CheckedItem(fact_key, statement, True, "numbers match")


def check(evidence: list[tuple[str, str]], facts: dict[str, str]) -> FaithfulnessResult:
    items = [check_item(k, s, facts) for k, s in evidence]
    score = sum(i.verified for i in items) / len(items) if items else 0.0
    return FaithfulnessResult(items=items, score=score)
