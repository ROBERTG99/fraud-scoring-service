"""Repository paths, overridable with FRAUD_REPO_ROOT inside containers."""

from __future__ import annotations

import os
from pathlib import Path


def repo_root() -> Path:
    env = os.environ.get("FRAUD_REPO_ROOT")
    if env:
        return Path(env)
    here = Path(__file__).resolve()
    for parent in here.parents:
        if (parent / "schemas" / "events").is_dir():
            return parent
    return Path.cwd()
