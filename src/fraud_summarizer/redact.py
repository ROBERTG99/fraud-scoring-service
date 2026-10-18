"""PII redaction before any provider call (spec §15): emails, phone numbers, card numbers (Luhn) and SSNs.

Synthetic identifiers (account tokens, salted device hashes, /24 IP prefixes, UUIDs) are not PII and are kept.

Numbers are not matched with one regular expression per kind: adjacent numbers ("+44 20 7946 0958 4111 1111
1111 1111") would merge into a span that is valid as neither and be left in place (M6 review). Instead each
run of digit groups is split into groups, every card-shaped and phone-shaped window of consecutive groups is
a candidate, and the non-overlapping set that redacts the most digits wins. Text is NFKC-normalised first
(fullwidth digits, no-break and thin spaces), with zero-width characters removed and Unicode dashes folded,
and redaction repeats until nothing changes. Phone groups may be separated by spaces, "-", "." or "/" only, so
lists of numbers in prose are kept. Card groups may be separated by any run of up to 8 characters that are not
letters or digits: a second pass redacts every Luhn-valid window of groups in a card layout (overlapping
windows merge into one label), and of the run pass and this pass in either order, the result that leaves fewer
digits is kept (security audit round 4). Percent escapes and HTML entities that stand for a digit, a space, "@"
or plain punctuation are decoded, never into letters, quotes, brackets or control characters, so decoding
cannot change what a prompt says or close its facts fence (round 6). Out of scope: names, postal addresses, IBANs, obfuscated addresses
("john(at)example"), masked card numbers and bare 9-digit SSNs without an "SSN" keyword (docs/security.md).
"""

from __future__ import annotations

import functools
import html
import re
import unicodedata
from dataclasses import dataclass

CARD_LABEL, PHONE_LABEL = "[REDACTED_CARD]", "[REDACTED_PHONE]"

# Bounded local part and labels keep the pattern linear (no catastrophic backtracking on long text).
EMAIL = re.compile(
    r"(?<![\w.%+-])[\w.%+-]{1,64}(?:@|%40)(?:[\w-]{1,63}(?:\.[\w-]{1,63})*\.[^\W\d_]{2,24}(?![\w-])"
    r"|localhost\b|\[[0-9A-Fa-f.:]{2,45}\])"
)
SSN = re.compile(r"(?<!\d)\d{3}[- .]\d{2}[- .]\d{4}(?!\d)|(?i:\bssn\b|social security)\W{0,3}\d{9}(?!\d)")
# Dates and times are not phone numbers: ISO dates, optionally followed by a valid time that does not run on
# into more ":NN" groups (a card written in pairs after a date, round-3 audit).
DATE_TIME = re.compile(
    r"\b\d{4}-\d{2}-\d{2}(?:[ T](?:[01]\d|2[0-3]):[0-5]\d(?::[0-5]\d(?:\.\d+)?)?(?:Z|[+-]\d{2}:\d{2})?(?!:?\d))?\b"
    r"|\b\d{4}/\d{1,2}/\d{1,2}\b|\b\d{1,2}/\d{1,2}/\d{4}\b"
)
# IPv4 addresses and prefixes are not phone numbers (facts carry /24 prefixes, spec §15).
IPV4 = re.compile(r"(?<![\d.])(?:\d{1,3}\.){3}\d{1,3}(?:/\d{1,2})?(?![\d.])")
# Salted hashes (hex, 32+ characters) and UUIDs are identifiers, not numbers: a digit run inside one can
# pass Luhn by chance.
HEX_ID = re.compile(
    r"(?<![0-9A-Za-z])(?:[0-9a-fA-F]{32,}|[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})(?![0-9A-Za-z])"
)
# A run of digit groups: optional leading "+", groups optionally in parentheses, separated by whitespace (one
# or two characters) or by one of - . / _ , : with at most one space either side. The surrounding characters
# are judged in _redact_run; which separators a card or a phone accepts is decided in _is_card and _is_phone.
SEP = r"(?:[ \t\r\n]{1,3}|[ \t]?[-./_,:|;*]{1,2}[ \t]?)"
RUN = re.compile(r"\+?\(?\d+\)?(?:(?:" + SEP + r"|(?<=\)))\(?\d+\)?)*")
PHONE_SEPS = frozenset({"", " ", "-", ".", "/", "_"})
# A contiguous 13-19 digit number glued to letters ("PAN4111...", "...1111EXP") is still a card; so is a
# separated one in a card shape, and a 3-3-4 phone (_card_shaped, _phone_shaped).
GLUED_CARD = re.compile(r"(?<!\d)\d{13,21}(?!\d)")  # up to 21: two digits may be a "%XX" escape
EXTENSION = re.compile(re.escape(PHONE_LABEL) + r"\s*(?:x|ext\.?|extension)\s*\d{1,6}\b", re.IGNORECASE)
DASHES = dict.fromkeys(
    [ord(c) for c in "\u2010\u2011\u2012\u2013\u2014\u2015\u2212\ufe58\ufe63\uff0d"], ord("-")
)
FULL_STOPS = dict.fromkeys([ord(c) for c in "\u3002\uff0e\uff61"], ord("."))
CARD_SHAPES = (
    (4, 4, 4, 4),
    (4, 4, 4, 4, 3),
    (4, 6, 5),
    (4, 6, 4),
    (4, 4, 5),
    (4, 4, 4, 3),
    (4, 4, 4, 1),
    (4, 3, 3, 3),
)
# Whitespace that is not a space, tab, CR or LF (vertical tab, form feed, NEL, line and paragraph separators).
EXOTIC_SPACE = dict.fromkeys([0x0B, 0x0C, 0x1C, 0x1D, 0x1E, 0x1F, 0x85, 0x2028, 0x2029], ord("\n"))
# A run of 20+ digits holds a Luhn-valid 13-19 digit window almost always: it is redacted whole.
LONG_DIGITS = re.compile(r"(?<!\d)\d{20,}(?!\d)")
# The wide card pass: digit groups (not part of a held placeholder) joined by up to 8 characters that are not
# letters, digits or the placeholder delimiter.
WIDE_RUN = re.compile(r"(?<![\d\x00])\d+(?:[^0-9A-Za-z\x00]{1,8}\d+)+(?![\d\x00])")
# Layouts the wide pass accepts besides CARD_SHAPES and equal groups (which the run pass knows too).
WIDE_SHAPES = frozenset({(4, 4, 8), (8, 8), (4, 12), (6, 10), (6, 13), (6, 6, 4)})


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


def normalise(text: str) -> str:
    """NFKC, zero-width and other format characters removed, Unicode dashes and full stops folded."""
    text = unicodedata.normalize("NFKC", text)
    text = "".join(ch for ch in text if unicodedata.category(ch) != "Cf")
    # NUL is dropped too: it delimits the placeholders of held identifiers in _once.
    text = text.translate(DASHES).translate(FULL_STOPS).translate(EXOTIC_SPACE)
    # "%40" is not decoded here (it would take two digits of a card separated by "%"): EMAIL matches it.
    return text.replace("\x00", "")


@dataclass(frozen=True)
class _Group:
    start: int  # offset of the group (including a leading "+" or "(") in the run
    end: int
    digits: str
    opens: bool  # starts with "+" or "(": the start of a phone number
    sep: str  # separator before this group: "" (first), " " (whitespace), the punctuation, or it + "~" if spaced
    plus: bool  # starts with "+": a phone, never a card ("(" may open either)


def _groups(run: str) -> list[_Group]:
    out: list[_Group] = []
    for m in re.finditer("(" + SEP + r")?(\+?\(?\d+\)?)", run):
        token, raw = m.group(2), m.group(1) or ""
        punct = raw.strip()
        sep = (punct + "~" if punct != raw else punct) if punct else (" " if raw else "")
        out.append(
            _Group(m.start(2), m.end(2), re.sub(r"\D", "", token), token[0] in "+(", sep, token[0] == "+")
        )
    return out


def _is_card(gs: list[_Group]) -> bool:
    digits = "".join(g.digits for g in gs)
    if not (13 <= len(digits) <= 19 and luhn_ok(digits)) or gs[0].plus:
        return False
    if len(gs) == 1:
        return True
    seps = {g.sep for g in gs[1:]}
    shape = tuple(len(g.digits) for g in gs)
    # A card shape may mix separators ("4111-1111 1111-1111"); other layouts need one separator throughout.
    return shape in CARD_SHAPES or (
        len(seps) == 1
        and (
            (all(len(g.digits) >= 3 for g in gs) and " " not in seps)
            or len(set(shape)) == 1  # equal groups: "4 1 1 1 ...", "41 11 11 ..."
        )
    )


def _is_phone(gs: list[_Group]) -> bool:
    if any(g.sep not in PHONE_SEPS for g in gs[1:]):
        return False  # commas, colons and spaced punctuation separate list items, not phone groups
    digits = "".join(g.digits for g in gs)
    lengths = [len(g.digits) for g in gs]
    # "0044 20 7946 0958": the international prefix 00 opens a number as "+" does.
    international = (
        len(gs) > 1 and gs[0].digits.startswith("00") and len(gs[0].digits) >= 3 and not gs[0].opens
    )
    if international and 8 <= len(digits) - 2 <= 15 and all(n <= 8 for n in lengths[1:]):
        return True
    # A later group may open only as the area code or trunk right after "+country": "+1 (415) ...", "+44 (0)20".
    if any(g.opens for g in gs[2:]) or (len(gs) > 1 and gs[1].opens and not gs[0].opens):
        return False
    if gs[0].opens:
        return 8 <= len(digits) <= 15 and all(n <= 8 for n in lengths[1:])
    if len(gs) == 1:
        return len(digits) in (10, 11)  # a bare national number, optionally with its country code
    spaced = any(g.sep == " " for g in gs[1:])
    if not spaced:  # one chunk: 555-014-2398, 415.555.2671, 1-800-555-0199, 555-0142
        if lengths == [3, 4]:
            return True
        return 8 <= len(digits) <= 15 and lengths[0] <= 4 and all(2 <= n <= 5 for n in lengths[1:])
    # Groups separated by spaces with no "+": the NANP 3-3-4 shape or a national number with trunk 0.
    if lengths == [3, 3, 4]:
        return True
    return gs[0].digits.startswith("0") and len(digits) in (10, 11) and all(2 <= n <= 6 for n in lengths)


def _uniform(gs: list[_Group]) -> str | None:
    seps = {g.sep for g in gs[1:]}
    return seps.pop() if len(seps) == 1 else None


def _card_shaped(gs: list[_Group]) -> bool:
    """A card layout even when glued to letters ("PAN4111 1111 1111 1111", "PAN4111-1111 1111-1111")."""
    return len(gs) > 1 and tuple(len(g.digits) for g in gs) in CARD_SHAPES


def _phone_shaped(gs: list[_Group]) -> bool:
    """A 3-3-4 number, even when glued to letters: one separator ("ph415-555-2671"), or an area code in
    parentheses with any of space, "-", "." or none after it ("Tel(415) 555-2671", "ph(415)555-2671")."""
    if [len(g.digits) for g in gs] != [3, 3, 4] or gs[0].plus or any(g.opens for g in gs[1:]):
        return False
    if gs[0].opens:
        return all(g.sep in ("", " ", "-", ".") for g in gs[1:])
    return _uniform(gs) in (" ", "-", ".")


MAX_WINDOW_DIGITS = 19


def _windows(gs: list[_Group], glued: bool) -> list[tuple[int, int, str]]:
    """Choose non-overlapping card/phone windows (group index ranges) covering the most digits."""
    n = len(gs)
    # Score: digits covered, then fewer windows, then more card windows (ties: one card over two phones).
    best: list[tuple[tuple[int, int, int], list[tuple[int, int, str]]]] = [((0, 0, 0), [])] * (n + 1)
    for i in range(n - 1, -1, -1):
        cand = best[i + 1]
        digits = 0
        for j in range(i + 1, n + 1):
            digits += len(gs[j - 1].digits)
            if digits > MAX_WINDOW_DIGITS and j > i + 1:
                break  # no card (19) or phone (17 with "00") is longer: the DP stays linear in the groups
            window = gs[i:j]
            label = None
            if _is_card(window) and (not glued or j - i == 1 or _card_shaped(window)):
                label = CARD_LABEL
            elif (not glued or _phone_shaped(window)) and _is_phone(window):
                label = PHONE_LABEL
            if label:
                rest = best[j][0]
                score = (
                    sum(len(g.digits) for g in window) + rest[0],
                    rest[1] - 1,
                    rest[2] + (label == CARD_LABEL),
                )
                if score > cand[0]:
                    cand = (score, [(i, j, label), *best[j][1]])
        best[i] = cand
    return best[0][1]


def _redact_run(m: re.Match[str]) -> str:
    run, text, start, end = m.group(0), m.string, m.start(), m.end()
    before = text[start - 1] if start else ""
    after = text[end : end + 2]
    # Glued to letters (UUIDs, hex hashes, "PAN4111..."): only a contiguous card number may be redacted.
    # "_" glues only to a word ("Tel_415..."), not to a redaction label or punctuation before it ("]_(588) ...").
    glued_before = before.isalpha() or (before == "_" and start >= 2 and text[start - 2].isalnum())
    glued = (
        not run.startswith("+")
        and (  # a "+" opens a phone number even after letters ("Tel+1415...")
            glued_before or (after[:1].isalpha() and not re.match(r"[xX]\d", after))
        )
    )
    gs = _groups(run)
    chosen = _windows(gs, glued)
    tail = re.match(r"[A-Za-z]*", text[end:]).group(0)  # type: ignore[union-attr]  # always matches
    hex_tail = all(c in "abcdefABCDEF" for c in tail)  # "4914904690ca" is a hex identifier, not a phone
    if (
        not chosen
        and glued
        and not glued_before
        and not hex_tail
        and len(gs) == 1
        and len(gs[0].digits) in (10, 11)
    ):
        chosen = [(0, 1, PHONE_LABEL)]  # a bare national number followed by letters: "4155552671card"
    if not chosen:
        return run
    out, pos = [], 0
    for i, j, label in chosen:
        out.append(run[pos : gs[i].start])
        out.append(label)
        pos = gs[j - 1].end
    out.append(run[pos:])
    return "".join(out)


def _wide_card_layout(shape: tuple[int, ...]) -> bool:
    return 13 <= sum(shape) <= 19 and (shape in CARD_SHAPES or shape in WIDE_SHAPES or len(set(shape)) == 1)


@functools.cache
def _layouts(k: int) -> tuple[tuple[int, ...], ...]:
    """The card layouts with k groups: card shapes, the wide pass's extra shapes and equal groups."""
    fixed = [shape for shape in (*CARD_SHAPES, *sorted(WIDE_SHAPES)) if len(shape) == k]
    equal = [(n,) * k for n in range(1, 20) if 13 <= n * k <= 19]
    return tuple(dict.fromkeys(fixed + equal))


def _layout_digits(run: str, window: list[tuple[int, int, bool]], shape: tuple[int, ...]) -> str | None:
    """The window's digits read in this layout: a group right after "%" may start with the two hex digits of an
    escape ("%40", "%20") or with card digits, and the layout says which (M6 review round 5). One check per
    layout, so the cost is linear in the groups (round 6: every combination of readings was tried)."""
    out = []
    for (a, b, pct), n in zip(window, shape, strict=True):
        if b - a == n:
            out.append(run[a:b])
        elif pct and b - a - 2 == n:
            out.append(run[a + 2 : b])
        else:
            return None
    return "".join(out)


def _wide_cards(m: re.Match[str], merge: bool = True) -> str:
    """Redact the Luhn-valid windows of groups in a card layout, joined by wide separators. With merge, every
    such window is redacted and overlapping windows merge into one label, so a window that passes Luhn by
    chance next to a card cannot leave the card's digits (security audit round 5; errs toward removing
    digits); without, the shortest windows from the left, not overlapping, so a chance window does not take a
    phone's area code (the caller keeps the better result)."""
    run, base, text = m.group(0), m.start(), m.string
    groups = []
    for g in re.finditer(r"\d+", run):
        a, b = g.span()
        pct = text[base + a - 1 : base + a] == "%" and b - a >= 3
        groups.append((a, b, pct))
    spans: list[list[int]] = []
    i = 0
    while i < len(groups):
        found = None
        for j in range(i + 1, len(groups)):
            window = groups[i : j + 1]
            if sum(b - a - 2 * pct for a, b, pct in window) > MAX_WINDOW_DIGITS:
                break
            if any(
                _wide_card_layout(shape)
                and (d := _layout_digits(run, window, shape)) is not None
                and luhn_ok(d)
                for shape in _layouts(len(window))
            ):
                found = j
                a, b = groups[i][0], groups[j][1]
                if spans and a <= spans[-1][1]:
                    spans[-1][1] = max(spans[-1][1], b)
                else:
                    spans.append([a, b])
                if not merge:
                    break
        i = found + 1 if found is not None and not merge else i + 1
    out, pos = [], 0
    for a, b in spans:
        out += [run[pos:a], CARD_LABEL]
        pos = b
    out.append(run[pos:])
    return "".join(out)


def _glued_card(m: re.Match[str]) -> str:
    d = m.group(0)
    escaped = m.string[m.start() - 1 : m.start()] == "%"  # "pay%404111...": "40" is the escape's
    card = (len(d) <= 19 and luhn_ok(d)) or (escaped and 13 <= len(d) - 2 <= 19 and luhn_ok(d[2:]))
    return CARD_LABEL if card else d


def _once(text: str) -> str:
    text = EMAIL.sub("[REDACTED_EMAIL]", text)
    text = SSN.sub("[REDACTED_SSN]", text)
    kept: list[str] = []

    def _hold(m: re.Match[str]) -> str:
        kept.append(m.group(0))
        return f"\x00{len(kept) - 1}\x00"

    def _hold_hex(m: re.Match[str]) -> str:
        # A hex run that contains a Luhn-valid 13-19 digit window may be a card glued to hex: not held, so the
        # card is redacted (and a hash that matches by chance loses those digits: the safe direction).
        if any(luhn_ok(w) for w in _digit_windows(m.group(0))):
            return m.group(0)
        return _hold(m)

    text = HEX_ID.sub(_hold_hex, text)
    for pattern in (DATE_TIME, IPV4):  # identifiers that contain digits but are not PII
        text = pattern.sub(_hold, text)
    text = LONG_DIGITS.sub(
        lambda m: CARD_LABEL if any(map(luhn_ok, _digit_windows(m.group(0)))) else m.group(0), text
    )
    text = GLUED_CARD.sub(_glued_card, text)
    # The run pass and the wide card pass in both orders (the wide pass merging every card window, or taking
    # the shortest ones), keeping the result that leaves the fewest digits: a phone window must not take the
    # first groups of a card the wide pass would redact whole, and a wide card window (Luhn-valid by chance)
    # must not take a card's digits or a phone's area code. Ties keep the run pass first.
    narrow_first = WIDE_RUN.sub(_wide_cards, RUN.sub(_redact_run, text))
    wide_first = RUN.sub(_redact_run, WIDE_RUN.sub(_wide_cards, text))
    shortest_first = RUN.sub(_redact_run, WIDE_RUN.sub(lambda m: _wide_cards(m, merge=False), text))
    text = min((narrow_first, wide_first, shortest_first), key=_digits_left)
    text = EXTENSION.sub(PHONE_LABEL, text)
    return re.sub(r"\x00(\d+)\x00", lambda m: kept[int(m.group(1))], text)


def _digits_left(text: str) -> int:
    return sum(c.isdigit() for c in re.sub(r"\x00\d+\x00", "", text))


def _digit_windows(s: str) -> list[str]:
    """Every 13-19 digit substring of each run of consecutive digits in s."""
    out = []
    for run in re.findall(r"\d{13,}", s):
        out += [run[i : i + n] for n in range(13, 20) for i in range(len(run) - n + 1)]
    return out


def _fixpoint(text: str) -> str:
    out = text
    for _ in range(4):  # a redaction can expose a neighbour (e.g. a phone after a card); repeat to a fixpoint
        nxt = _once(out)
        if nxt == out:
            break
        out = nxt
    return out


# What a percent escape or an HTML entity may decode to: digits, spaces and punctuation that only separates.
# Letters, quotes, brackets, "<", ">", backticks, backslashes, "%", "&" and control characters stay encoded, so
# decoding cannot change what a prompt says or close its facts fence (M6 review round 6).
SAFE_DECODED = frozenset("0123456789 @-./,:;|*+_~#!?=^$")
ESCAPE = re.compile(r"%([0-9A-Fa-f]{2})|&(#[0-9]{1,7};|#[xX][0-9A-Fa-f]{1,6};|[A-Za-z][A-Za-z0-9]{1,31};)")


def _decode_safe(m: re.Match[str]) -> str:
    if m.group(1) is not None:
        ch = chr(int(m.group(1), 16))
    else:
        ch = unicodedata.normalize("NFKC", html.unescape("&" + m.group(2)))  # "&nbsp;" is a space
    return ch if ch in SAFE_DECODED else m.group(0)


def decode_escapes(text: str) -> str:
    return ESCAPE.sub(_decode_safe, text)


def redact_text(text: str) -> str:
    """Two candidates, keeping the one that leaves fewer digits: redact as written, then decode what is left and
    redact again; or decode, then redact. "%40" is ambiguous: an "@" in "pay%404111..." but a separator before
    the card group "4046" in "...~%4046...", so neither order alone is safe (rounds 5 and 6)."""
    plain = normalise(text)
    first = _fixpoint(decode_escapes(_fixpoint(plain)))
    decoded = decode_escapes(plain)
    if decoded == plain:
        return first
    return min((first, _fixpoint(decoded)), key=_digits_left)


def contains_pii(text: str) -> bool:
    return redact_text(text) != decode_escapes(normalise(text))
