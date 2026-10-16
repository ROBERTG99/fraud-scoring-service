"""PII redaction before any provider call (spec §15): emails, phone numbers and card-like numbers (Luhn).

Synthetic identifiers (account tokens, salted device hashes, /24 IP prefixes) are not PII and are kept.
"""

from __future__ import annotations

import re

EMAIL = re.compile(r"[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}")
# Card-like: 13-19 digits, optionally separated by spaces or dashes.
CARD = re.compile(r"(?<![\w.])(?:\d[ -]?){12,18}\d(?![\w.])")
PHONE = re.compile(r"(?<![\w.])\+?\(?\d{1,4}\)?(?:[ .-]?\(?\d{2,4}\)?){2,5}(?![\w.])")


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
    # Phone numbers need separators or a leading +; bare digit runs are amounts, counts or timestamps.
    if 8 <= len(digits) <= 15 and (raw.startswith("+") or re.search(r"[ .()-]", raw)):
        return "[REDACTED_PHONE]"
    return raw


def redact_text(text: str) -> str:
    text = EMAIL.sub("[REDACTED_EMAIL]", text)
    text = CARD.sub(_card, text)
    return PHONE.sub(_phone, text)


def contains_pii(text: str) -> bool:
    return redact_text(text) != text
