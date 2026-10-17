"""PII redaction before any provider call (spec §15): emails, phone numbers and card-like numbers (Luhn).

Synthetic identifiers (account tokens, salted device hashes, /24 IP prefixes) are not PII and are kept.
"""

from __future__ import annotations

import re

# US SSN shape (labelled separately so it is not mistaken for a phone number).
SSN = re.compile(r"(?<![\w-])\d{3}-\d{2}-\d{4}(?![\w-])")
# Dates and times are not phone numbers: ISO dates, optionally followed by a time.
DATE_TIME = re.compile(
    r"\b\d{4}-\d{2}-\d{2}(?:[ T]\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}:\d{2})?)?\b"
)
EMAIL = re.compile(r"[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}")
# Card-like: 13-19 digits, optionally separated by spaces, dashes or dots. A sentence-ending dot may follow.
CARD = re.compile(r"(?<![\w.])(?:\d[ .-]?){12,18}\d(?!\w|\.\d)")
PHONE = re.compile(r"(?<![\w.])\+?\(?\d{1,4}\)?(?:[ .-]?\(?\d{2,4}\)?){2,5}(?!\w|\.\d)")


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


def _card(m: re.Match[str]) -> str:
    digits = re.sub(r"\D", "", m.group(0))
    return "[REDACTED_CARD]" if 13 <= len(digits) <= 19 and luhn_ok(digits) else m.group(0)


def _phone(m: re.Match[str]) -> str:
    raw = m.group(0)
    digits = re.sub(r"\D", "", raw)
    # Phone numbers need separators or a leading +, except a bare run of 10 or 11 digits (a national number,
    # optionally with its country code). Shorter or longer bare runs are amounts, counts or epoch milliseconds;
    # no fact carries a 10-11 digit number (amounts stop at 8 digits, timestamps are ISO 8601).
    if 8 <= len(digits) <= 15 and (raw.startswith("+") or re.search(r"[ .()-]", raw)):
        return "[REDACTED_PHONE]"
    if raw.isdigit() and len(raw) in (10, 11):
        return "[REDACTED_PHONE]"
    return raw


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
