"""PII redaction before any provider call (spec §15): emails, phone numbers and card-like numbers (Luhn).

Synthetic identifiers (account tokens, salted device hashes, /24 IP prefixes) are not PII and are kept.
"""

from __future__ import annotations

import re
from collections.abc import Callable

# US SSN shape (labelled separately so it is not mistaken for a phone number).
SSN = re.compile(r"(?<![\w-])\d{3}-\d{2}-\d{4}(?![\w-])")
# Dates and times are not phone numbers: ISO dates, optionally followed by a time.
DATE_TIME = re.compile(
    r"\b\d{4}-\d{2}-\d{2}(?:[ T]\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}:\d{2})?)?\b"
)
EMAIL = re.compile(r"[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}")
# Card-like: 13-19 digits, optionally separated by spaces, dashes or dots. A sentence-ending dot may follow.
CARD = re.compile(r"(?<![\w.])(?:\d[ .-]?){12,18}\d(?!\w|\.\d)")
# Up to 12 groups so that adjacent numbers are matched whole and split by _in_runs, never cut mid-number.
PHONE = re.compile(r"(?<![\w.])\+?\(?\d{1,4}\)?(?:[ .-]?\(?\d{2,4}\)?){2,12}(?!\w|\.\d)")


def luhn_ok(digits: str) -> bool:
    total, parity = 0, len(digits) % 2
    for i, ch in enumerate(digits):
        d = int(ch)
        if i % 2 == parity:
            d *= 2
            if d > 9:
                d -= 9
        total += d
    return total % 10 == 0


def _is_card(raw: str) -> bool:
    digits = re.sub(r"\D", "", raw)
    return 13 <= len(digits) <= 19 and luhn_ok(digits)


def _is_phone(raw: str) -> bool:
    digits = re.sub(r"\D", "", raw)
    # Phone numbers need separators or a leading +, except a bare run of 10 or 11 digits (a national number,
    # optionally with its country code). Shorter or longer bare runs are amounts, counts or epoch milliseconds;
    # no fact carries a 10-11 digit number (amounts stop at 8 digits, timestamps are ISO 8601).
    if 8 <= len(digits) <= 15 and (raw.startswith("+") or re.search(r"[ .()-]", raw) is not None):
        return True
    return raw.isdigit() and len(raw) in (10, 11)


def _in_runs(raw: str, valid: Callable[[str], bool], label: str, *, opens: bool = False) -> str:
    """Redact raw if valid; otherwise redact each longest valid run of its space-separated groups.

    The patterns repeat greedily, so adjacent numbers ("+44 20 7946 0958 5550142398") can merge into one
    span that is valid as neither; leaving that span in place would leak both (T-42). With opens=True (phones)
    a run of several groups must start where a number opens (a leading + or parenthesis), so the first groups
    of a longer digit run (a card number failing Luhn) are not mistaken for a phone number.
    """
    if valid(raw):
        return label
    parts = re.split(r"(\s+)", raw)
    groups, seps = parts[0::2], parts[1::2]
    out: list[str] = []
    i = 0
    while i < len(groups):
        j = i + 1
        for end in range(len(groups), i, -1):
            several = end - i > 1
            opened = [g.startswith(("+", "(")) for g in groups[i:end]]
            if several and opens and (not opened[0] or any(opened[1:])):
                continue  # a phone run starts where a number opens and ends before the next one opens
            candidate = "".join(
                g + sep for g, sep in zip(groups[i:end], [*seps[i : end - 1], ""], strict=True)
            )
            if valid(candidate):
                out.append(label)
                j = end
                break
        else:
            out.append(groups[i])
        if j < len(groups):
            out.append(seps[j - 1])
        i = j
    return "".join(out)


def _card(m: re.Match[str]) -> str:
    return _in_runs(m.group(0), _is_card, "[REDACTED_CARD]")


def _phone(m: re.Match[str]) -> str:
    return _in_runs(m.group(0), _is_phone, "[REDACTED_PHONE]", opens=True)


def redact_text(text: str) -> str:
    text = EMAIL.sub("[REDACTED_EMAIL]", text)
    text = SSN.sub("[REDACTED_SSN]", text)
    text = CARD.sub(_card, text)
    # Protect ISO dates/times from the phone pattern, then restore them.
    kept: list[str] = []

    def _hold(m: re.Match[str]) -> str:
        kept.append(m.group(0))
        return f"\x00{len(kept) - 1}\x00"

    text = DATE_TIME.sub(_hold, text)
    text = PHONE.sub(_phone, text)
    return re.sub(r"\x00(\d+)\x00", lambda m: kept[int(m.group(1))], text)


def contains_pii(text: str) -> bool:
    return redact_text(text) != text
