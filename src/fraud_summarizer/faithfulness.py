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
import unicodedata
from dataclasses import dataclass

# Digits that continue an identifier (tx_count_10m, mcc_novel_30d, R003) are not numbers; anything else is,
# including numbers with a unit or suffix attached ("12x", "30min", "940USD") and leading-dot decimals (".5").
NUMBER = re.compile(r"(?<![A-Za-z_\d.])-?(?:(?:\d{1,3}(?:,\d{3})+|\d+)(?:\.\d+)?|\.\d+)")


# Facts whose value is free text written by third parties, not a code, number or timestamp. Every other fact
# is a code, enum or validated value, whose comma pieces may be cited one by one: a fact that could hold
# third-party text must be listed here, or its pieces could carry clearing words (M6 review round 5).
FREE_TEXT_SUFFIXES = (".merchant_name",)
MIN_QUOTE_CHARS = 4
# Language that clears a case. A summary describes evidence and checks; it never recommends the outcome.
_CLEARING_WORDS = (
    r"approv\w*|authori[sz]\w*|unblock\w*|whitelist\w*|allowlist\w*|benign|dismiss\w*|reputable"
    r"|releas\w* (?:the )?(?:payment|funds|hold|transaction)|let (?:it|the \w+) (?:through|go)"
    r"|(?:clear|close) (?:the )?(?:case|alert)|not (?:a )?fraud\w*|non fraud\w*|no fraud|false positive\w*|legitimate"
    r"|genuine|trusted|safe to|legit|false alarm\w*|ok(?:ay)? to (?:proceed|pay|approve|go)|checks out"
    r"|verified (?:partner|merchant|customer|account|seller)"
)
# Words are delimited by anything but a letter or digit ("_approve_" too), and the words of a phrase may be
# joined by any run of characters that are not letters or digits, or by none ("false-positive", "false_alarm",
# "false.......positive", "not\u2010fraud", "falsealarm" once a zero-width space is removed; rounds 5 to 7).
# The run is possessive, so it cannot backtrack; statements and checks are at most a few hundred characters.
CLEARING = re.compile(
    r"(?<![^\W_])(?:" + _CLEARING_WORDS.replace(" ", r"[\W_]*+") + r")(?![^\W_])", re.IGNORECASE
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


def _token(value: str) -> tuple[re.Pattern[str], bool]:
    """A pattern for value as a whole token, and whether it is a code (matched with its case: "US" is not
    "us", REVIEW is not "review"; round 3 review)."""
    value = " ".join(value.split())
    code = value == value.upper() and any(c.isalpha() for c in value)
    needle = value if code else _norm(value)
    return re.compile(rf"(?<![0-9A-Za-z_]){re.escape(needle)}(?![0-9A-Za-z_])"), code


def _names(statement: str, value: str) -> bool:
    """value occurs in statement as a whole token (not inside a word: "US" in "status")."""
    pattern, code = _token(value)
    return pattern.search(" ".join(statement.split()) if code else _norm(statement)) is not None


def _first_spans(text: str, values: list[str]) -> list[tuple[int, int]]:
    """The span of the first whole-token occurrence of each value in text: one cited occurrence per value."""
    spans = []
    for value in values:
        _, code = _token(value)
        needle = re.escape(" ".join(value.split()))
        flags = 0 if code else re.IGNORECASE
        m = re.search(rf"(?<![0-9A-Za-z_]){needle}(?![0-9A-Za-z_])", text, flags)
        if m:
            spans.append(m.span())
    return spans


def _clears_outside(text: str, spans: list[tuple[int, int]]) -> bool:
    """Whether clearing language starts at any word of text and does not lie wholly inside one span. Every
    word start is tried, so a match inside a cited value cannot hide one that runs past it."""
    for i, ch in enumerate(text):
        if not ch.isalnum() or (i > 0 and text[i - 1].isalnum()):
            continue
        m = CLEARING.match(text, i)
        if m and not any(a <= m.start() and m.end() <= b for a, b in spans):
            return True
    return False


def _blank(text: str, spans: list[tuple[int, int]]) -> str:
    out = list(text)
    for a, b in spans:
        out[a:b] = " " * (b - a)
    return "".join(out)


# Hangul fillers render as nothing but are letters to the regex engine (security audit round 7).
_HANGUL_FILLERS = frozenset("\u115f\u1160\u3164\uffa0")


def _screenable(text: str) -> str:
    """NFKC with format characters (zero-width spaces, bidi marks), combining marks that stand alone after NFKC
    and Hangul fillers removed, so none of them can split a word."""
    text = unicodedata.normalize("NFKC", text)
    return "".join(
        ch
        for ch in text
        if unicodedata.category(ch) not in ("Cf", "Mn", "Me", "Mc") and ch not in _HANGUL_FILLERS
    )


def screen_checks(checks: list[str]) -> list[str]:
    """Recommended checks without case-clearing language (see CLEARING)."""
    return [c for c in checks if not CLEARING.search(_screenable(c))]


def check_item(fact_key: str, statement: str, facts: dict[str, str]) -> CheckedItem:
    if fact_key not in facts:
        return CheckedItem(fact_key, statement, False, "fact_key not in facts")
    raw = facts[fact_key]
    text = _screenable(statement)
    value = _as_number(raw)
    free_text = fact_key.endswith(FREE_TEXT_SUFFIXES)
    items = [i for i in [raw, *raw.split(",")] if i.strip()] if value is None else []
    # One occurrence of the fact's value is set aside: a merchant called "Genuine Parts Co" may be quoted, and
    # an IP prefix or version number may be cited. Clearing language anywhere else, a repeat of the value
    # included, drops the statement, and so do numbers beyond the fact. Free text is set aside whole, never by
    # its comma pieces: a merchant named "Approve" or "Shop, not fraud" must not let the statement say it
    # again (M6 review round 4, security audit round 4 M1).
    t = " ".join(text.split())
    spans = _first_spans(t, [raw] if free_text else items)
    rest = _blank(t, spans)
    if _clears_outside(t, spans) or CLEARING.search(" ".join(rest.split())):
        return CheckedItem(fact_key, statement, False, "clears the case")
    if value is None:
        if free_text:
            long_enough = len(_norm(raw).replace(" ", "")) >= MIN_QUOTE_CHARS
            carried = long_enough and _norm(raw) in _norm(text)
        else:
            carried = any(_names(text, item) for item in items)
        if not carried:
            return CheckedItem(fact_key, statement, False, "value not in statement")
        if numbers_in(rest):
            return CheckedItem(fact_key, statement, False, "statement has numbers beyond the fact")
        return CheckedItem(fact_key, statement, True, "carries the value")
    nums = numbers_in(text)
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
