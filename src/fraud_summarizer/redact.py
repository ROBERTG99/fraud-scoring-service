"""PII redaction before any provider call: emails, phone numbers, card numbers (Luhn) and SSNs.

Synthetic identifiers (account tokens, salted device hashes, /24 IP prefixes, UUIDs) are not PII and are kept.

Numbers are not matched with one regular expression per kind: adjacent numbers ("+44 20 7946 0958 4111 1111
1111 1111") would merge into a span that is valid as neither and be left in place (review). Instead each
run of digit groups is split into groups, every card-shaped and phone-shaped window of consecutive groups is
a candidate, and the non-overlapping set that redacts the most digits wins. Text is NFKC-normalised first
(fullwidth digits, no-break and thin spaces), with zero-width characters removed and Unicode dashes folded,
and redaction repeats until nothing changes. Phone groups may be separated by spaces, "-", "." or "/" only, so
lists of numbers in prose are kept. Card groups may be separated by any run of up to 8 characters that are not
letters or digits: a second pass redacts every Luhn-valid window of groups in a card layout (overlapping
windows merge into one label), and of the run pass and this pass in either order, the result that leaves fewer
digits is kept (security audit round 4). Percent escapes (UTF-8 sequences as one character) and HTML entities
are decoded, to a fixpoint, into digits, "@" and plain punctuation; any other character that is not a letter
becomes a space and letters stay encoded, so decoding cannot change what a prompt says or close its facts
fence. The text is redacted as written and as decoded, and whatever either reading finds is redacted (rounds
5 to 7). Out of scope: names, postal addresses, IBANs, obfuscated addresses ("john(at)example"), masked card
numbers and bare 9-digit SSNs without an "SSN" keyword (docs/security.md).
"""

from __future__ import annotations

import asyncio
import collections
import contextvars
import functools
import hashlib
import html
import html.entities
import json
import re
import threading
import unicodedata
from collections.abc import Callable
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from typing import Any

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
# IPv4 addresses and prefixes are not phone numbers (facts carry /24 prefixes).
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
# Each label is followed by one private-use character that numbers the text it replaced (_label), so redact_text
# can map labels back to spans; normalise removes private-use characters from the input.
MARK = "[\U000f0000-\U000ffffd]"
EXTENSION = re.compile(
    re.escape(PHONE_LABEL) + MARK + r"?\s*(?:x|ext\.?|extension)\s*\d{1,6}\b", re.IGNORECASE
)
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


_REPLACED: contextvars.ContextVar[list[str] | None] = contextvars.ContextVar("redact_replaced", default=None)


def _label(label: str, original: str) -> str:
    """label, numbered with the text it replaces when redact_text tracks spans."""
    replaced = _REPLACED.get()
    if replaced is None:
        return label
    if len(replaced) >= 0xFFFFD - 0xF0000:
        return label  # out of numbers: the spans cannot be recovered and the caller fails closed
    replaced.append(original)
    return label + chr(0xF0000 + len(replaced) - 1)


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
    text = "".join(ch for ch in text if unicodedata.category(ch) not in ("Cf", "Co"))
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
    # Score: digits covered, then fewer windows, then more card windows (ties: one card over two phones). Each
    # position keeps a back-pointer (the window taken there, or None to skip a group), so choosing stays linear
    # in the groups; copying the chosen list at every step made long runs quadratic (security audit round 8).
    score: list[tuple[int, int, int]] = [(0, 0, 0)] * (n + 1)
    choice: list[tuple[int, str] | None] = [None] * (n + 1)
    for i in range(n - 1, -1, -1):
        best, pick = score[i + 1], None
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
                rest = score[j]
                cand = (digits + rest[0], rest[1] - 1, rest[2] + (label == CARD_LABEL))
                if cand > best:
                    best, pick = cand, (j, label)
        score[i], choice[i] = best, pick
    out: list[tuple[int, int, str]] = []
    i = 0
    while i < n:
        c = choice[i]
        if c is None:
            i += 1
            continue
        out.append((i, c[0], c[1]))
        i = c[0]
    return out


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
        out.append(_label(label, run[gs[i].start : gs[j - 1].end]))
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
    escape ("%40", "%20") or with card digits, and the layout says which (review round 5). One check per
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
        out += [run[pos:a], _label(CARD_LABEL, run[a:b])]
        pos = b
    out.append(run[pos:])
    return "".join(out)


def _glued_card(m: re.Match[str]) -> str:
    d = m.group(0)
    escaped = m.string[m.start() - 1 : m.start()] == "%"  # "pay%404111...": "40" is the escape's
    card = (len(d) <= 19 and luhn_ok(d)) or (escaped and 13 <= len(d) - 2 <= 19 and luhn_ok(d[2:]))
    return _label(CARD_LABEL, d) if card else d


def _once(text: str) -> str:
    text = EMAIL.sub(lambda m: _label("[REDACTED_EMAIL]", m.group(0)), text)
    text = SSN.sub(lambda m: _label("[REDACTED_SSN]", m.group(0)), text)
    kept: list[str] = []
    replaced = _REPLACED.get()
    first = len(replaced) if replaced is not None else 0

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
        lambda m: (
            _label(CARD_LABEL, m.group(0)) if any(map(luhn_ok, _digit_windows(m.group(0)))) else m.group(0)
        ),
        text,
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
    text = EXTENSION.sub(lambda m: _label(PHONE_LABEL, m.group(0)), text)

    def restore(t: str) -> str:
        return re.sub(r"\x00(\d+)\x00", lambda m: kept[int(m.group(1))], t)

    if replaced is not None:  # the replaced texts recorded in this call may hold placeholders too
        replaced[first:] = [restore(t) for t in replaced[first:]]
    return restore(text)


_DIGIT = re.compile(r"\d")
_PLACEHOLDER = re.compile(r"\x00\d+\x00")


def _digits_left(text: str) -> int:
    return len(_DIGIT.findall(_PLACEHOLDER.sub("", text)))


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


# Escapes are decoded to a fixpoint as written, then folded once: a decoded digit, "@" or one of these separators
# stays, a format character or combining mark goes, a decoded letter keeps the text it was written as, and any
# other character (spaces, controls, quotes, brackets, "<", ">", backticks, backslashes, "%", "&") becomes a
# space, so decoding cannot change what a prompt says or close its facts fence (rounds 6 to 8).
SAFE_DECODED = frozenset("0123456789@-./,:;|*+_~#!?=^$")
_PERCENT = r"%[0-9A-Fa-f]{2}"
# Real named references only (with ";", or the legacy names browsers and html.unescape also read without it),
# longest first, and numeric references of any length, with or without ";" (rounds 8 and 9: "&notx;" names
# nothing; read as one escape it kept growing, so it is "&not" followed by "x;").
_NAMES = sorted(html.entities.html5, key=len, reverse=True)
_ENTITY = r"&(?:#[0-9]+;?|#[xX][0-9A-Fa-f]+;?|(?:" + "|".join(map(re.escape, _NAMES)) + "))"
ESCAPE_RUN = re.compile(rf"(?:{_PERCENT}|{_ENTITY})+")
_UNIT = re.compile(rf"(?P<pct>(?:{_PERCENT})+)|(?P<ent>{_ENTITY})")


def _units(run: str) -> list[tuple[int, int, str | None]]:
    """The escapes of a run as (start, end, decoded): a percent run as UTF-8 characters (an invalid or cut byte
    as U+FFFD, as unquote(errors="replace") reads it), an entity as html.unescape reads it (None when it names
    nothing)."""
    out: list[tuple[int, int, str | None]] = []
    for m in _UNIT.finditer(run):
        if m.group("ent"):
            text = html.unescape(m.group(0))
            out.append((m.start(), m.end(), None if text == m.group(0) else text))
            continue
        data = bytes(int(m.group(0)[k + 1 : k + 3], 16) for k in range(0, len(m.group(0)), 3))
        k = 0
        while k < len(data):
            n = 1 if data[k] < 0x80 else 2 if data[k] >> 5 == 6 else 3 if data[k] >> 4 == 14 else 4
            try:
                ch = data[k : k + n].decode("utf-8")
            except UnicodeDecodeError:
                ch, n = "\ufffd", 1
            out.append((m.start() + 3 * k, m.start() + 3 * (k + n), ch))
            k += n
    return out


def _decode_raw_once(text: str) -> tuple[str, list[tuple[int, int]]]:
    """text with its escapes decoded once and not folded, so a decoded "%" or "&" can start the next escape,
    and each result character's span in text."""
    out: list[str] = []
    spans: list[tuple[int, int]] = []
    pos = 0
    for m in ESCAPE_RUN.finditer(text):
        for i in range(pos, m.start()):
            out.append(text[i])
            spans.append((i, i + 1))
        for ua, ub, raw in _units(m.group(0)):
            a, b = ua + m.start(), ub + m.start()
            for i, ch in enumerate(raw if raw is not None else text[a:b]):
                out.append(ch)
                spans.append((a, b) if raw is not None else (a + i, a + i + 1))
        pos = m.end()
    for i in range(pos, len(text)):
        out.append(text[i])
        spans.append((i, i + 1))
    return "".join(out), spans


class _DecodeError(ValueError):
    """Decoding did not converge; redaction fails closed."""


def _decode(text: str) -> tuple[str, list[tuple[int, int]]]:
    """Escapes decoded to a fixpoint and folded, with each result character's span in text. Folding can make a
    new escape ("&#&sup1;" folds to "&#1"), so decoding and folding repeat until the result is stable (round 8:
    a second redaction must change nothing)."""
    out, spans = _decode_and_fold(text)
    # Every round that changes the text makes it shorter (an escape becomes one character, or keeps its text for
    # a letter), so this ends within len(text) rounds; there is no other bound to run out (round 9).
    while ESCAPE_RUN.search(out):
        nxt, local = _decode_and_fold(out)
        if nxt == out:
            break
        if len(nxt) >= len(out):  # not expected: the caller fails closed rather than loop
            raise _DecodeError("decoding did not shorten the text")
        spans = [(spans[a][0], spans[b - 1][1]) for a, b in local]
        out = nxt
    return out, spans


def _decode_and_fold(text: str) -> tuple[str, list[tuple[int, int]]]:
    """Escapes decoded to a fixpoint as written, then folded once, with each result character's span in text. A
    decoding round makes the text shorter or leaves it as it is, so this ends. Adjacent decoded spaces become
    one."""
    raw, spans = text, [(i, i + 1) for i in range(len(text))]
    while True:
        nxt, local = _decode_raw_once(raw)
        if nxt == raw:
            break
        spans = [(spans[a][0], spans[b - 1][1]) for a, b in local]
        raw = nxt
    out: list[str] = []
    out_spans: list[tuple[int, int]] = []
    last_letter: tuple[int, int] | None = None
    for ch, (a, b) in zip(raw, spans, strict=True):
        if b - a == 1 and text[a] == ch:  # as written, not decoded
            out.append(ch)
            out_spans.append((a, b))
            continue
        folded = normalise(ch).translate(EXOTIC_SPACE)
        if any(f.isalnum() and not f.isdecimal() for f in folded):
            # A letter, or a character whose NFKC form holds one ("\u3219" is "(하)"), keeps the text it was
            # written as, once per escape and with nothing around it, so the text never grows (round 9).
            if last_letter != (a, b):
                out.extend(text[a:b])
                out_spans.extend((i, i + 1) for i in range(a, b))
                last_letter = (a, b)
            continue
        for f in folded:
            if f in SAFE_DECODED or f.isdecimal():  # digits of any script are digits to the redactor too
                out.append(f)
                out_spans.append((a, b))
            elif out and out[-1] == " " and out_spans[-1][1] >= a and out_spans[-1][1] - out_spans[-1][0] > 1:
                out_spans[-1] = (out_spans[-1][0], max(out_spans[-1][1], b))  # "%0D%0A" is one separator
            else:
                out.append(" ")
                out_spans.append((a, b))
    return "".join(out), out_spans


def decode_escapes(text: str) -> str:
    """Escapes decoded to a fixpoint and folded (see _decode), without the span bookkeeping when there is
    nothing to decode."""
    return _decode(text)[0] if ESCAPE_RUN.search(text) else text


_LABELLED = re.compile(r"(\[REDACTED_[A-Z]+\])(" + MARK + ")")


def _expand(text: str, replaced: list[str]) -> tuple[str, list[tuple[int, int, str]]]:
    """The text a redaction started from, rebuilt from its labels, and each top-level label's span in it."""
    out: list[str] = []
    spans: list[tuple[int, int, str]] = []
    size = 0
    pos = 0

    def full(t: str, depth: int) -> str:
        if depth > 64:
            raise ValueError("labels nested too deep")
        return _LABELLED.sub(lambda m: full(replaced[ord(m.group(2)) - 0xF0000], depth + 1), t)

    for m in _LABELLED.finditer(text):
        out.append(text[pos : m.start()])
        size += m.start() - pos
        original = full(replaced[ord(m.group(2)) - 0xF0000], 0)
        spans.append((size, size + len(original), m.group(1)))
        out.append(original)
        size += len(original)
        pos = m.end()
    out.append(text[pos:])
    return "".join(out), spans


def _redaction_spans(text: str) -> list[tuple[int, int, str]] | None:
    """The spans of text that _fixpoint redacts, with their labels; None if they cannot be recovered."""
    replaced: list[str] = []
    token = _REPLACED.set(replaced)
    try:
        out = _fixpoint(text)
    finally:
        _REPLACED.reset(token)
    try:
        rebuilt, spans = _expand(out, replaced)
    except (ValueError, IndexError):
        return None
    return spans if rebuilt == text else None


_LABEL_RANK = {"[REDACTED_CARD]": 0, "[REDACTED_SSN]": 1, "[REDACTED_PHONE]": 2, "[REDACTED_EMAIL]": 3}


def redact_text(text: str) -> str:
    """See _redact. Results for short texts are cached: each fact value is redacted by build_facts, the redact
    step and the gateway in turn, and the later passes usually see the same string (redaction is a pure
    function). The cache is keyed by a SHA-256 of the text, so it holds no raw card number (round 8)."""
    if len(text) > _CACHED_CHARS:
        return _redact(text)
    key = hashlib.sha256(text.encode("utf-8", "surrogatepass")).digest()
    with _CACHE_LOCK:
        hit = _CACHE.get(key)
        if hit is not None:
            _CACHE.move_to_end(key)
            return hit
    out = _redact(text)
    with _CACHE_LOCK:
        _CACHE[key] = out
        if len(_CACHE) > _CACHE_SIZE:
            _CACHE.popitem(last=False)
    return out


_CACHED_CHARS, _CACHE_SIZE = 4096, 8192  # fact values, and the system prompt the gateway sees on every call
_CACHE: collections.OrderedDict[bytes, str] = collections.OrderedDict()
_CACHE_LOCK = threading.Lock()


def _cache_clear() -> None:
    with _CACHE_LOCK:
        _CACHE.clear()


def _redact(text: str) -> str:
    """Redact what the text says as written or as decoded, and repeat on the result until nothing changes: a
    label can cut a run so that the gap beside it holds a card neither reading saw (round 8)."""
    try:
        return _redact_to_fixpoint(text)
    except _DecodeError:  # not expected: fail closed on the text as written
        return _closed(_fixpoint(normalise(text)))


def _redact_to_fixpoint(text: str) -> str:
    out, labelled = _redact_once(text)
    # A pass that changes the output removes digits, so this ends within as many rounds as there are digits.
    while labelled:  # nothing was cut, so the decoded reading, decoded to the end, has seen every run
        nxt, labelled = _redact_once(out)
        if nxt == out:
            break
        if len(_DIGIT.findall(nxt)) >= len(_DIGIT.findall(out)):  # not expected: fail closed
            return _closed(nxt)
        out = nxt
    return out


def _closed(text: str) -> str:
    """Fail closed: every stretch of digits joined by non-letters that holds four or more digits is redacted."""
    return re.sub(
        r"\d[\d\W_]*\d|\d",
        lambda m: CARD_LABEL if sum(c.isdigit() for c in m.group(0)) >= 4 else m.group(0),
        text,
    )


def _redact_once(text: str) -> tuple[str, bool]:
    """Both readings are redacted, their spans mapped onto the text as written and merged, and the rest
    decoded. "%40" is an "@" in "pay%404111..." but may also be a separator before the card group "4046" in
    "...~%4046...", so neither reading alone is safe, and choosing between their results let one reading's
    find go unredacted (rounds 5 to 7). Also says whether anything was labelled."""
    plain = normalise(text)
    decoded, origin = _decode(plain) if ESCAPE_RUN.search(plain) else (plain, [])
    spans = _redaction_spans(plain)
    decoded_spans = _redaction_spans(decoded) if decoded != plain else []
    if spans is None or decoded_spans is None:  # not expected: fail closed (round 8)
        return _closed(_fixpoint(decoded)), False
    for a, b, label in decoded_spans:
        spans.append((origin[a][0], origin[b - 1][1], label))
    merged: list[list[Any]] = []
    for a, b, label in sorted(spans):
        if merged and a < merged[-1][1]:
            merged[-1][1] = max(merged[-1][1], b)
            merged[-1][2] = min(merged[-1][2], label, key=_LABEL_RANK.__getitem__)
        else:
            merged.append([a, b, label])
    out, pos = [], 0
    for a, b, label in merged:
        out += [decode_escapes(plain[pos:a]), label]
        pos = b
    out.append(decode_escapes(plain[pos:]))
    return "".join(out), bool(merged)


def contains_pii(text: str) -> bool:
    return redact_text(text) != decode_escapes(normalise(text))


# A fenced JSON block of a prompt (facts.render, judge.judge_message): its values are redacted one by one and
# the block written again, so the gateway never redacts JSON syntax ("\u0060" for a backtick) as text (round 7).
_BLOCK = re.compile(r"(<(?P<tag>[a-z_]+)>\s*```json\s*)(?P<body>\{.*?\})(\s*```\s*</(?P=tag)>)", re.DOTALL)


def _redact_json(value: Any) -> Any:
    if isinstance(value, str):
        return redact_text(value)
    if isinstance(value, list):
        return [_redact_json(v) for v in value]
    if isinstance(value, dict):
        return {k: _redact_json(v) for k, v in value.items()}
    return value


def redact_prompt(text: str) -> str:
    """redact_text for a prompt: each fenced JSON block value by value, written again as facts.render writes
    it, and the text around the blocks as text."""
    out, pos = [], 0
    for m in _BLOCK.finditer(text):
        out.append(redact_text(text[pos : m.start()]))
        try:
            data = json.loads(m.group("body"))
        except json.JSONDecodeError:
            out.append(redact_text(m.group(0)))
        else:
            body = json.dumps(_redact_json(data), indent=1, ensure_ascii=False)
            body = body.replace("<", "\\u003c").replace(">", "\\u003e").replace("`", "\\u0060")
            out.append(m.group(1) + body + m.group(4))
        pos = m.end()
    out.append(redact_text(text[pos:]))
    return "".join(out)


# One long-lived pool for redaction: off the event loop the console and the consumer share (security audit
# round 7), and not the loop's default executor, which asyncio.run creates and shuts down on every run.
_POOL = ThreadPoolExecutor(max_workers=2, thread_name_prefix="redact")


async def off_loop[T](fn: Callable[[], T]) -> T:
    """Run CPU-bound redaction in the redaction pool and await its result."""
    return await asyncio.get_running_loop().run_in_executor(_POOL, fn)
