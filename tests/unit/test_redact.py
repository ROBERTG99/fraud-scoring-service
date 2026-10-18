from __future__ import annotations

import random
import re

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
        ("ssn 123-45-6789", "ssn [REDACTED_SSN]"),
        ("card 4111.1111.1111.1111 used", "card [REDACTED_CARD] used"),
        ("call 4155550100", "call [REDACTED_PHONE]"),
        ("call 14155550100.", "call [REDACTED_PHONE]."),
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
        "changed at 2026-10-16 09:58:00",
        "event_ts 2026-09-20T10:03:00.000Z",
        "amount 10000000 minor units",
        "device prefix 123456789012",
        "ratio 1.0526 and score 0.7692",
        "ref 123456789",
    ],
)
def test_keeps_non_pii(text: str) -> None:
    assert redact_text(text) == text


def test_luhn() -> None:
    assert luhn_ok("4111111111111111") and not luhn_ok("4111111111111112")


@pytest.mark.parametrize(
    ("text", "expected"),
    [
        # Adjacent numbers must not merge into one rejected span that is then left in place.
        ("+44 20 7946 0958 (555) 014-2398", "[REDACTED_PHONE] [REDACTED_PHONE]"),
        ("call +44 20 7946 0958 5550142398", "call [REDACTED_PHONE] [REDACTED_PHONE]"),
        ("4111 1111 1111 1111 5550142398", "[REDACTED_CARD] [REDACTED_PHONE]"),
        ("ref 94000 then 4111 1111 1111 1111", "ref 94000 then [REDACTED_CARD]"),
    ],
)
def test_adjacent_pii_is_each_redacted(text: str, expected: str) -> None:
    assert redact_text(text) == expected


@pytest.mark.parametrize(
    ("text", "expected"),
    [
        # M6 review: a card right after a phone was swallowed by a card match starting inside the phone.
        ("+44 20 7946 0958 4111 1111 1111 1111", "[REDACTED_PHONE] [REDACTED_CARD]"),
        ("4459629161571603 252-545-6434", "[REDACTED_CARD] [REDACTED_PHONE]"),
        ("555-014-2398 555-014-2399", "[REDACTED_PHONE] [REDACTED_PHONE]"),
        ("555-014-2398-4111-1111-1111-1111", "[REDACTED_PHONE]-[REDACTED_CARD]"),
        ("ip 203.0.113.0/24 version 2026.10.2", "ip 203.0.113.0/24 version 2026.10.2"),
    ],
)
def test_adjacent_cards_and_phones(text: str, expected: str) -> None:
    assert redact_text(text) == expected


def _card(rng: random.Random) -> str:
    while True:
        d = "4" + "".join(rng.choice("0123456789") for _ in range(15))
        if luhn_ok(d):
            break
    sep = rng.choice([" ", "-", ".", "", ",", ":", " - ", ", ", "\t"])
    return sep.join(d[i : i + 4] for i in range(0, 16, 4)) if sep else d


def _phone(rng: random.Random) -> str:
    a, b, c = ("".join(rng.choice("0123456789") for _ in range(n)) for n in (3, 3, 4))
    forms = [
        f"({a}) {b}-{c}",
        f"{a}-{b}-{c}",
        f"{a}{b}{c}",
        f"+44 20 {b}{c[:1]} {c}",
        f"+1 {a} {b} {c}",
        f"{a}.{b}.{c}",
        f"0044 20 {b}{c[:1]} {c}",
    ]
    return rng.choice(forms)


def test_no_digit_of_adjacent_pii_survives() -> None:
    """Seeded fuzz: two PII items side by side, in any order and format, next to an amount that must stay."""
    rng = random.Random(7)
    for _ in range(3000):
        items = [rng.choice((_card, _phone))(rng) for _ in range(2)]
        text = "amount 94000 then " + rng.choice((" ", ", ", " / ")).join(items) + " end"
        out = redact_text(text)
        assert re.sub(r"\D", "", out) == "94000", (text, out)


@pytest.mark.parametrize(
    "text",
    [
        # Security audit M6 (H3): layouts that reached the provider.
        "PAN4111111111111111",
        "SQ *JOE'S CAFE 4111111111111111EXP",
        "Ref 4111111111111111_2",
        "4111 1111 1111 1111",
        "4111 1111 1111 1111",
        "4111–1111–1111–1111",
        "4111  1111  1111  1111",
        "4111/1111/1111/1111",
        "4111_1111_1111_1111",
        "4\u200b111111111111111",
        "４１１１１１１１１１１１１１１１",
        "3782 822463 10005",
        "415 555 2671x9",
        "ph.415.555.2671",
        "+1/415/555/2671",
        "+1–415–555–2671",
        "555-0142",
        "+33 1 42 68 53 00",
        "+81-3-1234-5678",
        "+44 (0)20 7946 0958",
        "020 7946 0958",
        "07700 900123",
        "+91 98765 43210",
        "+1 (415) 555-2671 ext. 123",
        "123 45 6789",
        "SSN 123.45.6789",
        "ssn:123456789",
        "a123-45-6789",
        "jöhn@exämple.de",
        "john@example。com",
        "+44 20 7946 0958 4111 1111 1111 1111",
        "Tel+14155552671",
        "+49 30 901820",
        "john%40example.com",
        "user@localhost",
        "user@[192.168.1.1]",
    ],
)
def test_audit_layouts_leave_no_digits(text: str) -> None:
    out = redact_text(text)
    assert not re.search(r"\d{2,}", out), out  # a lone digit may remain ("Ref [REDACTED_CARD]_2")
    assert "@" not in out


@pytest.mark.parametrize(
    "text",
    [
        "0192f000-0000-7000-8000-000000000001",
        "device 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
        "ratio 0.873412345678",
        "rules_version 2026.10.2",
        "event_ts 2026-09-20T10:03:00.000+02:00",
        "acc_00004242 m_00042",
        "on 2026/10/17 12:34 or 17/10/2026",
    ],
)
def test_more_non_pii_is_kept(text: str) -> None:
    assert redact_text(text) == text


def test_email_pattern_is_linear() -> None:
    import time

    t0 = time.perf_counter()
    redact_text("a" * 20_000 + "@")
    assert time.perf_counter() - t0 < 0.5


def test_ip_prefixes_are_kept() -> None:
    assert redact_text("ip 64.192.210.0/24 and 10.0.0.1") == "ip 64.192.210.0/24 and 10.0.0.1"


@pytest.mark.parametrize(
    "text",
    [
        # M6 round 2 (security audit N1, code review): layouts that reached the provider.
        "4111111111111111deadbeefdeadbeef",
        "card 4111111111111111abcdefabcdefabcdefab",
        "4111,1111,1111,1111",
        "4111:1111:1111:1111",
        "4111\t1111\t1111\t1111",
        "4111\n1111\n1111\n1111",
        "4111 - 1111 - 1111 - 1111",
        "4111, 1111, 1111, 1111",
        "4 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1",
        "0044 20 7946 0958",
        "4155552671card",
    ],
)
def test_round_two_layouts_leave_no_digit_run(text: str) -> None:
    out = redact_text(text)
    assert not re.search(r"\d{2}", out), f"{text!r} -> {out!r}"
    assert not re.search(r"(\d\D?){6}", out.replace("[REDACTED_CARD]", "").replace("[REDACTED_PHONE]", "")), (
        out
    )


def test_input_cannot_collide_with_the_hold_placeholder() -> None:
    assert redact_text("\x007\x00 and \x000\x00") == "7 and 0"


@pytest.mark.parametrize(
    "text",
    [
        "amounts 1200, 3400 and 5600",  # a list of numbers is not a phone
        "Order 1234. 5678 items",
        "at 10:30, 3 items",
    ],
)
def test_lists_and_prose_are_kept(text: str) -> None:
    assert redact_text(text) == text
