"""PII redaction before any provider call (spec §15): emails, phone numbers, card numbers (Luhn) and SSNs.

Synthetic identifiers (account tokens, salted device hashes, /24 IP prefixes, UUIDs) are not PII and are kept.

Numbers are not matched with one regular expression per kind: adjacent numbers ("+44 20 7946 0958 4111 1111
1111 1111") would merge into a span that is valid as neither and be left in place (M6 review). Instead each
run of digit groups is split into groups, every card-shaped and phone-shaped window of consecutive groups is
a candidate, and the non-overlapping set that redacts the most digits wins. Text is NFKC-normalised first
(fullwidth digits, no-break and thin spaces), with zero-width characters removed and Unicode dashes folded,
and redaction repeats until nothing changes. Card groups may be separated by whitespace, "-", ".", "/", "_",
"," or ":" (optionally with spaces around them); phone groups by spaces, "-", "." or "/" only, so lists of
numbers in prose are kept. Out of scope: names, postal addresses, IBANs, obfuscated addresses
("john(at)example"), masked card numbers and bare 9-digit SSNs without an "SSN" keyword (docs/security.md).
"""

from __future__ import annotations

import re
import unicodedata
from dataclasses import dataclass

CARD_LABEL, PHONE_LABEL = "[REDACTED_CARD]", "[REDACTED_PHONE]"

# Bounded local part and labels keep the pattern linear (no catastrophic backtracking on long text).
EMAIL = re.compile(
    r"(?<![\w.%+-])[\w.%+-]{1,64}@(?:[\w-]{1,63}(?:\.[\w-]{1,63})*\.[^\W\d_]{2,24}(?![\w-])"
    r"|localhost\b|\[[0-9A-Fa-f.:]{2,45}\])"
)
SSN = re.compile(r"(?<!\d)\d{3}[- .]\d{2}[- .]\d{4}(?!\d)|(?i:\bssn\b|social security)\W{0,3}\d{9}(?!\d)")
# Dates and times are not phone numbers: ISO dates, optionally followed by a time.
DATE_TIME = re.compile(
    r"\b\d{4}-\d{2}-\d{2}(?:[ T]\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}:\d{2})?)?\b"
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
SEP = r"(?:[ \t\r\n]{1,2}|[ \t]?[-./_,:][ \t]?)"
RUN = re.compile(r"\+?\(?\d+\)?(?:(?:" + SEP + r"|(?<=\)))\(?\d+\)?)*")
PHONE_SEPS = frozenset({"", " ", "-", ".", "/", "_"})
# A contiguous 13-19 digit number glued to letters ("PAN4111...", "...1111EXP") is still a card.
GLUED_CARD = re.compile(r"(?<!\d)\d{13,19}(?!\d)")
EXTENSION = re.compile(re.escape(PHONE_LABEL) + r"\s*(?:x|ext\.?|extension)\s*\d{1,6}\b", re.IGNORECASE)
DASHES = dict.fromkeys(
    [ord(c) for c in "\u2010\u2011\u2012\u2013\u2014\u2015\u2212\ufe58\ufe63\uff0d"], ord("-")
)
FULL_STOPS = dict.fromkeys([ord(c) for c in "\u3002\uff0e\uff61"], ord("."))
CARD_SHAPES = ((4, 4, 4, 4), (4, 4, 4, 4, 3), (4, 6, 5), (4, 6, 4))


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
    return text.translate(DASHES).translate(FULL_STOPS).replace("%40", "@").replace("\x00", "")


@dataclass(frozen=True)
class _Group:
    start: int  # offset of the group (including a leading "+" or "(") in the run
    end: int
    digits: str
    opens: bool  # starts with "+" or "(": the start of a phone number
    sep: str  # separator before this group: "" (first), " " (whitespace), the punctuation, or it + "~" if spaced


def _groups(run: str) -> list[_Group]:
    out: list[_Group] = []
    for m in re.finditer("(" + SEP + r")?(\+?\(?\d+\)?)", run):
        token, raw = m.group(2), m.group(1) or ""
        punct = raw.strip()
        sep = (punct + "~" if punct != raw else punct) if punct else (" " if raw else "")
        out.append(_Group(m.start(2), m.end(2), re.sub(r"\D", "", token), token[0] in "+(", sep))
    return out


def _is_card(gs: list[_Group]) -> bool:
    digits = "".join(g.digits for g in gs)
    if not (13 <= len(digits) <= 19 and luhn_ok(digits)) or gs[0].opens:
        return False
    if len(gs) == 1:
        return True
    seps = {g.sep for g in gs[1:]}
    shape = tuple(len(g.digits) for g in gs)
    return len(seps) == 1 and (
        shape in CARD_SHAPES
        or (all(len(g.digits) >= 3 for g in gs) and " " not in seps)
        or len(set(shape)) == 1  # equal groups: "4 1 1 1 ...", "41 11 11 ..."
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


def _windows(gs: list[_Group], glued: bool) -> list[tuple[int, int, str]]:
    """Choose non-overlapping card/phone windows (group index ranges) covering the most digits."""
    n = len(gs)
    # Score: digits covered, then fewer windows, then more card windows (ties: one card over two phones).
    best: list[tuple[tuple[int, int, int], list[tuple[int, int, str]]]] = [((0, 0, 0), [])] * (n + 1)
    for i in range(n - 1, -1, -1):
        cand = best[i + 1]
        for j in range(i + 1, n + 1):
            window = gs[i:j]
            label = None
            if _is_card(window) and (not glued or j - i == 1):
                label = CARD_LABEL
            elif not glued and _is_phone(window):
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
    glued_before = before.isalpha() or before == "_"
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
    text = GLUED_CARD.sub(lambda m: CARD_LABEL if luhn_ok(m.group(0)) else m.group(0), text)
    text = RUN.sub(_redact_run, text)
    text = EXTENSION.sub(PHONE_LABEL, text)
    return re.sub(r"\x00(\d+)\x00", lambda m: kept[int(m.group(1))], text)


def _digit_windows(s: str) -> list[str]:
    """Every 13-19 digit substring of each run of consecutive digits in s."""
    out = []
    for run in re.findall(r"\d{13,}", s):
        out += [run[i : i + n] for n in range(13, 20) for i in range(len(run) - n + 1)]
    return out


def redact_text(text: str) -> str:
    out = normalise(text)
    for _ in range(4):  # a redaction can expose a neighbour (e.g. a phone after a card); repeat to a fixpoint
        nxt = _once(out)
        if nxt == out:
            break
        out = nxt
    return out


def contains_pii(text: str) -> bool:
    return redact_text(text) != normalise(text)
