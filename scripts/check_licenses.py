"""Licence check for `make security`.

Covers what ships or runs: the Go modules compiled into `./cmd/...` and `./internal/...`, every Python
distribution in `.venv` (runtime, training, serving and dev groups), and the console's production npm
dependencies (`pnpm licenses list --prod`). A declared licence (SPDX expression or trove classifier) is used
when it is specific; otherwise the bundled LICENSE text is classified. Exceptions are explicit and recorded in
docs/DECISIONS.md D-039: MPL-2.0 (file-level copyleft, used unmodified) and LGPL-3.0 for the psycopg driver
family only. Anything else that is not permissive, or that cannot be identified, fails.
"""

from __future__ import annotations

import importlib.metadata
import json
import os
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
PERMISSIVE = {
    "MIT", "MIT-0", "MIT-CMU", "Apache-2.0", "BSD-2-Clause", "BSD-3-Clause", "BSD-3-Clause-Clear", "0BSD",
    "ISC", "PSF-2.0", "Python-2.0", "Zlib", "CC0-1.0", "Unlicense", "BlueOak-1.0.0", "OFL-1.1",
}  # fmt: skip
EXCEPTIONS = {"MPL-2.0": None, "LGPL-3.0-only": {"psycopg", "psycopg-binary", "psycopg-pool"}}  # D-039
CLASSIFIERS = {
    "MIT License": "MIT",
    "MIT No Attribution License (MIT-0)": "MIT-0",
    "Apache Software License": "Apache-2.0",
    "BSD License": "BSD-3-Clause",
    "ISC License (ISCL)": "ISC",
    "Python Software Foundation License": "PSF-2.0",
    "Mozilla Public License 2.0 (MPL 2.0)": "MPL-2.0",
    "The Unlicense (Unlicense)": "Unlicense",
    "zlib/libpng License": "Zlib",
}
ALIASES = {
    "MIT License": "MIT",
    "Apache License 2.0": "Apache-2.0",
    "Apache 2.0": "Apache-2.0",
    "Apache License, Version 2.0": "Apache-2.0",
    "3-Clause BSD License": "BSD-3-Clause",
    "BSD": "BSD-3-Clause",
}
SPDX_TOKEN = re.compile(r"^[A-Za-z0-9.+-]+$")


def from_text(text: str) -> str | None:
    """SPDX id of a licence text, from its distinctive wording."""
    t = " ".join(text.split())
    if "GNU LESSER GENERAL PUBLIC LICENSE" in t.upper():
        return "LGPL-3.0-only" if "Version 3" in t else "LGPL-2.1"
    if "GNU GENERAL PUBLIC LICENSE" in t.upper() or "GNU AFFERO" in t.upper():
        return "GPL-3.0"
    if "Mozilla Public License" in t:
        return "MPL-2.0"
    if "Apache License" in t and "Version 2.0" in t:
        return "Apache-2.0"
    if "Permission is hereby granted, free of charge" in t:
        return "MIT"
    if "Redistribution and use in source and binary forms" in t:
        return (
            "BSD-3-Clause" if "Neither the name" in t or "names of its contributors" in t else "BSD-2-Clause"
        )
    if "Permission to use, copy, modify, and/or distribute" in t or "ISC License" in t:
        return "ISC"
    return None


def normalise(declared: str) -> str | None:
    """An SPDX expression for a declared licence, or None when it says too little."""
    d = declared.strip()
    if d.startswith("License ::"):
        return CLASSIFIERS.get(d.split("::")[-1].strip())
    d = ALIASES.get(d, d)
    tokens = [tok for tok in re.split(r"[()\s]+", d) if tok and tok not in ("AND", "OR", "WITH")]
    return d if tokens and all(SPDX_TOKEN.match(tok) for tok in tokens) else None


def _ok(lic: str, name: str) -> str:
    if lic in PERMISSIVE:
        return "ok"
    if lic in EXCEPTIONS and (EXCEPTIONS[lic] is None or name in (EXCEPTIONS[lic] or set())):
        return "exception"
    return "fail"


def verdict(name: str, spdx: str | None) -> str:
    """ok, exception (recorded in D-039) or fail, for one package's SPDX expression.

    A small SPDX parser: AND binds tighter than OR; OR takes the most permissive alternative, AND the least.
    """
    if not spdx:
        return "fail"
    rank = {"ok": 0, "exception": 1, "fail": 2}
    tokens = re.findall(r"\(|\)|[A-Za-z0-9.+-]+", spdx)
    pos = 0

    def peek() -> str | None:
        return tokens[pos] if pos < len(tokens) else None

    def take() -> str:
        nonlocal pos
        pos += 1
        return tokens[pos - 1]

    def term() -> str:
        tok = take()
        if tok == "(":
            v = any_of()
            if take() != ")":
                raise IndexError
            return v
        if tok in ("(", ")", "AND", "OR", "WITH"):
            raise IndexError
        if peek() == "WITH":  # a licence exception only widens the licence
            take()
            take()
        return _ok(tok, name)

    def all_of() -> str:
        v = term()
        while peek() == "AND":
            take()
            v = max(v, term(), key=rank.__getitem__)
        return v

    def any_of() -> str:
        v = all_of()
        while peek() == "OR":
            take()
            v = min(v, all_of(), key=rank.__getitem__)
        return v

    try:
        result = any_of()
    except IndexError:
        return "fail"
    return result if pos == len(tokens) else "fail"  # tokens left over: not a well-formed expression


def python_packages() -> list[tuple[str, str, str | None]]:
    out = []
    for dist in importlib.metadata.distributions():
        md = dist.metadata
        name = md["Name"]
        if name == "fraud-scoring-service":
            continue  # this repository
        declared = [md.get("License-Expression") or ""]
        declared += [c for c in md.get_all("Classifier") or [] if c.startswith("License ::")]
        declared.append(md.get("License") or "")
        spdx = next((s for d in declared if d and (s := normalise(d))), None)
        if spdx is None:
            for f in dist.files or []:
                if re.search(r"LICEN[CS]E|COPYING", f.name, re.I):
                    spdx = from_text(f.read_text() or "")
                    if spdx:
                        break
        out.append(("python", f"{name}=={md['Version']}", spdx))
    return out


def go_modules() -> list[tuple[str, str, str | None]]:
    env = {**os.environ, "GOTOOLCHAIN": "auto"}
    fmt = "{{with .Module}}{{if not .Main}}{{.Path}}@{{.Version}}\t{{.Dir}}{{end}}{{end}}"
    listed = subprocess.run(
        ["go", "list", "-deps", "-f", fmt, "./cmd/...", "./internal/..."],
        cwd=ROOT,
        env=env,
        capture_output=True,
        text=True,
        check=True,
    ).stdout
    out = []
    for line in sorted(set(listed.split("\n")) - {""}):
        mod, _, directory = line.partition("\t")
        files = [p for p in Path(directory).iterdir() if re.match(r"(LICEN[CS]E|COPYING)", p.name, re.I)]
        spdx = next((s for p in files if (s := from_text(p.read_text(errors="replace")))), None)
        out.append(("go", mod, spdx))
    return out


def npm_packages() -> list[tuple[str, str, str | None]]:
    listed = subprocess.run(
        ["pnpm", "licenses", "list", "--prod", "--json"],
        cwd=ROOT / "web",
        capture_output=True,
        text=True,
        check=True,
    ).stdout
    return [
        ("npm", f"{p['name']}@{','.join(p.get('versions', []))}", normalise(lic))
        for lic, pkgs in json.loads(listed).items()
        for p in pkgs
    ]


def main() -> int:
    rows = go_modules() + python_packages() + npm_packages()
    counts = {"ok": 0, "exception": 0, "fail": 0}
    for eco, pkg, spdx in rows:
        name = pkg.split("==")[0].split("@")[0].lower()
        v = verdict(name, spdx)
        counts[v] += 1
        if v != "ok":
            print(f"{v.upper():9} {eco:6} {pkg}: {spdx or 'unidentified'}")
    print(
        f"licences: {counts['ok']} permissive, {counts['exception']} recorded exceptions (D-039), {counts['fail']} failing"
    )
    return 1 if counts["fail"] else 0


if __name__ == "__main__":
    sys.exit(main())
