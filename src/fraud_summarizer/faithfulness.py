"""Faithfulness check without an LLM (spec §4.6).

An evidence item is verified when its `fact_key` exists in the case facts and the statement carries that
fact: a numeric fact's number (and no other number), a non-numeric fact's value or one of its comma-separated
items (a reason code, say), or, for free text written by third parties such as a merchant name, a quote of at
least 4 characters. Statements and recommended checks that steer toward clearing the case ("approve", "not
fraud", "verified merchant") are not verified: that is what injected text would ask for, and the analyst, not
the summary, decides (security audit M6 M2, round 2 N4). Score = verified / total. Unverified items are
dropped; when none remain the summary is `unfaithful`.
"""

from __future__ import annotations

import math
import re
from dataclasses import dataclass

# Digits that continue an identifier (tx_count_10m, mcc_novel_30d, R003) are not numbers; anything else is,
# including numbers with a unit or suffix attached ("12x", "30min", "940USD") and leading-dot decimals (".5").
NUMBER = re.compile(r"(?<![A-Za-z_\d.])-?(?:(?:\d{1,3}(?:,\d{3})+|\d+)(?:\.\d+)?|\.\d+)")


# Facts whose value is free text written by third parties, not a code, number or timestamp.
FREE_TEXT_SUFFIXES = (".merchant_name",)
MIN_QUOTE_CHARS = 4
# Language that clears a case. A summary describes evidence and checks; it never recommends the outcome.
CLEARING = re.compile(
    r"\b(?:approv\w*|authori[sz]\w*|unblock\w*|whitelist\w*|allowlist\w*|benign|dismiss\w*|reputable"
    r"|releas\w* (?:the )?(?:payment|funds|hold|transaction)|let (?:it|the \w+) (?:through|go)"
    r"|(?:clear|close) (?:the )?(?:case|alert)|not (?:a )?fraud\w*|no fraud|false positive|legitimate|genuine"
    r"|trusted|safe to"
    r"|verified (?:partner|merchant|customer|account|seller))\b",
    re.IGNORECASE,
)


def _norm(text: str) -> str:
    return " ".join(text.casefold().split())


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


def _names(statement: str, value: str) -> bool:
    """value occurs in statement as a whole token: not inside a word ("US" in "status"), and with its case
    when it is a code (upper case, digits, "_"), so "review" does not stand in for REVIEW (round 3 review)."""
    value = " ".join(value.split())
    code = value == value.upper() and any(c.isalpha() for c in value)
    hay, needle = (" ".join(statement.split()), value) if code else (_norm(statement), _norm(value))
    return re.search(rf"(?<![0-9A-Za-z_]){re.escape(needle)}(?![0-9A-Za-z_])", hay) is not None


def screen_checks(checks: list[str]) -> list[str]:
    """Recommended checks without case-clearing language (see CLEARING)."""
    return [c for c in checks if not CLEARING.search(c)]


def check_item(fact_key: str, statement: str, facts: dict[str, str]) -> CheckedItem:
    if fact_key not in facts:
        return CheckedItem(fact_key, statement, False, "fact_key not in facts")
    if CLEARING.search(statement):
        return CheckedItem(fact_key, statement, False, "clears the case")
    raw = facts[fact_key]
    if fact_key.endswith(FREE_TEXT_SUFFIXES):
        long_enough = len(_norm(raw).replace(" ", "")) >= MIN_QUOTE_CHARS
        quoted = long_enough and _norm(raw) in _norm(statement)
        return CheckedItem(
            fact_key, statement, quoted, "quotes the text" if quoted else "free text not quoted"
        )
    nums = numbers_in(statement)
    value = _as_number(raw)
    if value is None:
        if nums:
            return CheckedItem(
                fact_key, statement, False, "statement has numbers but the fact is not numeric"
            )
        named = any(_names(statement, item) for item in [raw, *raw.split(",")] if item.strip())
        return CheckedItem(
            fact_key, statement, named, "names the value" if named else "value not in statement"
        )
    if not nums:
        return CheckedItem(fact_key, statement, False, "statement does not carry the fact's number")
    for n in nums:
        if n != value:
            return CheckedItem(fact_key, statement, False, f"number {n:g} != fact value {value:g}")
    return CheckedItem(fact_key, statement, True, "numbers match")


def check(evidence: list[tuple[str, str]], facts: dict[str, str]) -> FaithfulnessResult:
    items = [check_item(k, s, facts) for k, s in evidence]
    score = sum(i.verified for i in items) / len(items) if items else 0.0
    return FaithfulnessResult(items=items, score=score)
