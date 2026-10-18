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


def _card(rng: random.Random, shapes: tuple[tuple[int, ...], ...] = ((4, 4, 4, 4),)) -> str:
    shape = rng.choice(shapes)
    while True:
        d = "4" + "".join(rng.choice("0123456789") for _ in range(sum(shape) - 1))
        if luhn_ok(d):
            break
    sep = rng.choice([" ", "-", ".", "", ",", ":", " - ", ", ", "\t"])
    if not sep:
        return d
    out, pos = [], 0
    for n in shape:
        out.append(d[pos : pos + n])
        pos += n
    return sep.join(out)


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
        text = "amount 94000 then " + rng.choice((" ", ", ", " / ", "_", "; ")).join(items) + " end"
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


def test_window_search_is_linear_in_the_groups(monkeypatch: pytest.MonkeyPatch) -> None:
    """Security audit round 4: "1;" x 1000 took 6.7 s (19 s with three candidate passes) because every window
    of a run was tried. Windows stop at 19 digits, so doubling a run doubles the windows tried, not more."""
    from fraud_summarizer import redact

    calls = 0
    is_card = redact._is_card

    def counting(gs: list[redact._Group]) -> bool:
        nonlocal calls
        calls += 1
        return is_card(gs)

    monkeypatch.setattr(redact, "_is_card", counting)
    for unit in ("1;", "1 ", "(1)"):
        counts = []
        for n in (200, 400):
            calls = 0
            redact_text(unit * n)
            counts.append(calls)
        assert counts[1] <= 2.2 * counts[0], (unit, counts)
        assert counts[1] <= 19 * 3 * 400 * 2, (unit, counts)  # at most 19 windows per group, per pass


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
        # M6 review round 3: separated numbers touching letters or "_".
        "PAN4111 1111 1111 1111",
        "Visa4111-1111-1111-1111",
        "PAN_4111 1111 1111 1111",
        "4111 1111 1111 1111EXP",
        "4111-1111-1111-1111exp",
        "ph415-555-2671",
        "Tel_415-555-2671",
        "call415 555 2671",
        # M6 review round 4: mixed separators touching letters.
        "Tel(415) 555-2671",
        "Call Us(415) 555-2671",
        "ph(415)555-2671",
        "ACME PAN4111-1111 1111-1111",
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


def test_no_digit_of_adjacent_pii_survives_in_every_card_shape() -> None:
    """Seeded fuzz over every card shape (16, 15 Amex, 19, 14 Diners) and every joiner, "_" included; the M6
    round-3 review found 39 leaks in 20,000 such pairs before cards glued by "_" were handled (D-058, D-061)."""
    shapes = ((4, 4, 4, 4), (4, 6, 5), (4, 4, 4, 4, 3), (4, 6, 4))
    rng = random.Random(11)
    for _ in range(20_000):
        items = [_card(rng, shapes) if rng.random() < 0.5 else _phone(rng) for _ in range(2)]
        text = "amount 94000 then " + rng.choice((" ", ", ", " / ", "_", "; ", "\n")).join(items) + " end"
        out = redact_text(text)
        assert re.sub(r"\D", "", out) == "94000", (text, out)


@pytest.mark.parametrize(
    "text",
    [
        # Security audit round 3 (finding 2).
        "(4111, 1111, 1111, 1111)",
        "(3782 - 822463 - 10005",  # Amex 4-6-5 opening with "("
        "4111|1111|1111|1111",
        "4111;1111;1111;1111",
        "4111*1111*1111*1111",
        "4111 -- 1111 -- 1111 -- 1111",
        "4111   1111   1111   1111",
        "4111\x0b1111\x0b1111\x0b1111",
        "4111\x0c1111\x0c1111\x0c1111",
        "4111\u00851111\u00851111\u00851111",
        "4111 1111 1111 1111",
        "4222 2222 2222 2",  # a 13-digit card written 4-4-4-1
        "4222 222 222 222",  # and 4-3-3-3
        "ref 0000000041111111111111110000000000",  # a card inside a long run of digits
    ],
)
def test_round_three_card_layouts_leave_no_digit_run(text: str) -> None:
    out = redact_text(text)
    assert not re.search(r"\d{2}", out), f"{text!r} -> {out!r}"


def test_no_card_survives_in_any_layout_or_context() -> None:
    """Seeded fuzz in the security auditor's round-3 shape: cards of 13-19 digits, split every common way and
    by every separator, between prefixes and suffixes that are themselves numbers, dates, IPs or hex. No 8 of
    the card's digits in a row may reach the output (D-063)."""
    rng = random.Random(23)
    seps = [" ", "-", ".", "/", "_", ",", ":", "\t", "\n", " - ", ", ", "  ", "\r\n", " : ", " / ", "|"]
    before = ["", "card ", "PAN: ", "x ", "Tel +44 20 7946 0958 ", "on 2024-05-01 ", "ip 10.1.2.0/24 ",
              "id deadbeefdeadbeefdeadbeefdeadbeef ", "amt 12.50, ", "(", '"', "#"]  # fmt: skip
    after = ["", " exp 12/29", " thanks", ", 415-555-2671", ")", '"', " 99", ".", " 2024-05-01"]
    splits = {13: [[4, 4, 5], [4, 4, 4, 1], [4, 3, 3, 3]], 14: [[4, 6, 4]], 15: [[4, 6, 5], [4, 4, 4, 3]],
              16: [[4, 4, 4, 4], [8, 8], [2] * 8], 19: [[4, 4, 4, 4, 3]]}  # fmt: skip
    for _ in range(5_000):
        n = rng.choice([13, 14, 15, 16, 16, 16, 17, 18, 19])
        while True:
            d = rng.choice("3456") + "".join(rng.choice("0123456789") for _ in range(n - 1))
            if luhn_ok(d):
                break
        sizes = rng.choice([[n], [1] * n, *splits.get(n, [])])
        groups, pos = [], 0
        for k in sizes:
            groups.append(d[pos : pos + k])
            pos += k
        text = rng.choice(before) + rng.choice(seps).join(groups) + rng.choice(after)
        digits = re.sub(r"\D", "", redact_text(text))
        assert not any(d[i : i + 8] in digits for i in range(n - 7)), (text, redact_text(text))


# Security audit round 4 (M2): any separator one step outside a fixed list leaked the whole card.
ROUND_FOUR_SEPARATORS = ["    ", "\t\t\t\t", "\n\n\n\n", "\r\n\r\n", "---", " --- ", "-\n", ".\n", ",\n", "  -  ", " ,  ",
                         "\u00b7", "\u2022", "~", "+", "=", "'", "#"]  # fmt: skip
SEPARATOR_CHARS = " \t\n\r-.,:;|*/_~+='#\u00b7\u2022()[]{}<>!?&%$^\"\\"


def test_no_card_survives_any_short_separator_run() -> None:
    """Seeded fuzz: card groups in a card shape, separated by any run of 1-8 non-alphanumeric characters (the
    same run at every gap, or a different one at each), between the round-3 prefixes and suffixes. No 8 of the
    card's digits in a row may reach the output (D-064)."""
    rng = random.Random(29)
    before = ["", "card ", "Card: ", "note: customer said ", "PAN", "Tel +44 20 7946 0958 ", "on 2024-05-01 ",
              "id deadbeefdeadbeefdeadbeefdeadbeef ", "amt 12.50, ", "(", '"']  # fmt: skip
    after = ["", " thanks", ".", "EXP", " exp 12/29", ", 415-555-2671", " 99", ")"]
    shapes = {13: [[4, 4, 5], [4, 3, 3, 3]], 14: [[4, 6, 4]], 15: [[4, 6, 5], [4, 4, 4, 3]],
              16: [[4, 4, 4, 4]], 19: [[4, 4, 4, 4, 3]]}  # fmt: skip
    for k in range(6_000):
        n = rng.choice([13, 14, 15, 16, 16, 16, 19])
        while True:
            d = rng.choice("3456") + "".join(rng.choice("0123456789") for _ in range(n - 1))
            if luhn_ok(d):
                break
        sizes = rng.choice(shapes[n])
        groups, pos = [], 0
        for size in sizes:
            groups.append(d[pos : pos + size])
            pos += size

        def sep() -> str:
            if rng.random() < 0.3:
                return rng.choice(ROUND_FOUR_SEPARATORS)
            return "".join(rng.choice(SEPARATOR_CHARS) for _ in range(rng.randint(1, 8)))

        uniform = sep()
        seps = [uniform if k % 2 else sep() for _ in groups[1:]]
        text = rng.choice(before) + groups[0] + "".join(s + g for s, g in zip(seps, groups[1:], strict=True))
        text += rng.choice(after)
        digits = re.sub(r"\D", "", redact_text(text))
        assert not any(d[i : i + 8] in digits for i in range(n - 7)), (text, redact_text(text))


@pytest.mark.parametrize("sep", ROUND_FOUR_SEPARATORS)
def test_round_four_separators_are_card_separators(sep: str) -> None:
    out = redact_text(f"card {sep.join(['4480', '4336', '5923', '9424'])} thanks")
    assert out == "card [REDACTED_CARD] thanks", repr(out)
