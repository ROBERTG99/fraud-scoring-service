from __future__ import annotations

import pytest

from fraud_summarizer.redact import contains_pii, luhn_ok, redact_text


@pytest.mark.parametrize(
    ("text", "expected"),
    [
        ("mail jane.doe+x@example.co.uk now", "mail [REDACTED_EMAIL] now"),
        ("card 4111 1111 1111 1111 used", "card [REDACTED_CARD] used"),
        ("card 4111-1111-1111-1111", "card [REDACTED_CARD]"),
        ("pan 5500005555555559", "pan [REDACTED_CARD]"),
        ("call +1 415 555 0100", "call [REDACTED_PHONE]"),
        ("call (020) 7946-0958", "call [REDACTED_PHONE]"),
    ],
)
def test_redacts_pii(text: str, expected: str) -> None:
    assert redact_text(text) == expected
    assert contains_pii(text)


@pytest.mark.parametrize(
    "text",
    [
        "amount 94000 minor units",
        "acc_00004242 on 203.0.113.0/24",
        "device " + "ab" * 32,
        "2026-09-20T10:03:00.000Z",
        "transaction 0192f000-0000-7000-8000-000000000001",
        "card 4111 1111 1111 1112 fails luhn",
        "1788000000123 ms",
    ],
)
def test_keeps_non_pii(text: str) -> None:
    assert redact_text(text) == text


def test_luhn() -> None:
    assert luhn_ok("4111111111111111") and not luhn_ok("4111111111111112")
