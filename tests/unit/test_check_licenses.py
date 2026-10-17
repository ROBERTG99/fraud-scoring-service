"""Licence check (spec §8, §17): permissive dependencies only; recorded exceptions are explicit."""

from __future__ import annotations

import pytest

from scripts import check_licenses as cl

MIT = "Permission is hereby granted, free of charge, to any person obtaining a copy of this software"
APACHE = (
    "                                 Apache License\n                           Version 2.0, January 2004"
)
BSD3 = "Redistribution and use in source and binary forms ... Neither the name of the copyright holder"
GPL = "GNU GENERAL PUBLIC LICENSE\n Version 3, 29 June 2007"


@pytest.mark.parametrize(
    ("text", "spdx"),
    [(MIT, "MIT"), (APACHE, "Apache-2.0"), (BSD3, "BSD-3-Clause"), (GPL, "GPL-3.0"), ("", None)],
)
def test_classifies_licence_text(text: str, spdx: str | None) -> None:
    assert cl.from_text(text) == spdx


@pytest.mark.parametrize(
    ("declared", "spdx"),
    [
        ("MIT", "MIT"),
        ("License :: OSI Approved :: MIT License", "MIT"),
        ("License :: OSI Approved :: BSD License", "BSD-3-Clause"),
        ("Apache-2.0 OR BSD-3-Clause", "Apache-2.0 OR BSD-3-Clause"),
        ("License :: OSI Approved", None),  # too vague: fall back to the licence text
    ],
)
def test_normalises_declared_licences(declared: str, spdx: str | None) -> None:
    assert cl.normalise(declared) == spdx


@pytest.mark.parametrize(
    ("name", "spdx", "verdict"),
    [
        ("anything", "MIT", "ok"),
        ("anything", "Apache-2.0 OR BSD-3-Clause", "ok"),
        ("anything", "MPL-2.0 AND (Apache-2.0 OR MIT)", "exception"),  # MPL used unmodified (D-039)
        ("psycopg", "LGPL-3.0-only", "exception"),
        ("someother", "LGPL-3.0-only", "fail"),  # LGPL is accepted only for the recorded package
        ("anything", "GPL-3.0", "fail"),
        ("anything", None, "fail"),
    ],
)
def test_verdicts(name: str, spdx: str | None, verdict: str) -> None:
    assert cl.verdict(name, spdx) == verdict
