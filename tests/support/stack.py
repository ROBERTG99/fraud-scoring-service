"""Fresh Postgres databases and Go binaries for Python integration tests."""

from __future__ import annotations

import os
import subprocess
import uuid
from collections.abc import Iterator
from contextlib import contextmanager
from dataclasses import dataclass
from pathlib import Path

import psycopg

ROOT = Path(__file__).resolve().parents[2]


def env(name: str) -> str:
    value = os.environ.get(name)
    if not value:
        raise RuntimeError(f"{name} is not set; run through `make test` (compose.test.yaml)")
    return value


def build_go(out_dir: Path, *names: str) -> dict[str, Path]:
    out = {}
    go_env = {**os.environ, "GOTOOLCHAIN": "auto"}
    for name in names:
        target = out_dir / name
        subprocess.run(["go", "build", "-o", str(target), f"./cmd/{name}"], cwd=ROOT, env=go_env, check=True)
        out[name] = target
    return out


@dataclass(frozen=True)
class Database:
    name: str
    owner_dsn: str  # role fraud (migrations, fixtures)
    app_dsn: str  # role fraud_app (scorer)
    summarizer_dsn: str  # role fraud_summarizer (case summarizer, D-043)


@contextmanager
def fresh_database(work: Path) -> Iterator[Database]:
    """A new database with every migration applied (via `scorer migrate`), dropped afterwards."""
    admin = env("FRAUD_TEST_PG_DSN")
    name = "py_" + uuid.uuid4().hex[:12]
    with psycopg.connect(admin, autocommit=True) as conn:
        conn.execute(f"CREATE DATABASE {name}")  # generated identifier
    base = admin.split("?", 1)[0].rsplit("/", 1)[0]
    owner = f"{base}/{name}?sslmode=disable"
    db = Database(
        name,
        owner,
        owner.replace("fraud:fraud@", "fraud_app:fraud_app@", 1),
        owner.replace("fraud:fraud@", "fraud_summarizer:fraud_summarizer@", 1),
    )
    try:
        scorer = work / "scorer"
        if not scorer.exists():
            build_go(work, "scorer")
        subprocess.run(
            [str(scorer), "migrate"], env={**os.environ, "FRAUD_PG_MIGRATE_DSN": owner}, check=True
        )
        yield db
    finally:
        with psycopg.connect(admin, autocommit=True) as conn:
            conn.execute(f"DROP DATABASE IF EXISTS {name} WITH (FORCE)")
