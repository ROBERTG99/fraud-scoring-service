"""Schemathesis contract test: the running scorer (gRPC + gateway) against gen/openapi/fraud.yaml.

Runs the real `cmd/scorer` and `cmd/dev-issuer` binaries against the compose.test.yaml stack (a fresh
database per run). The server-streaming /v1/metrics/live endpoint is covered by Go tests instead.
"""

from __future__ import annotations

import json
import os
import socket
import subprocess
import time
import urllib.error
import urllib.request
import uuid
from collections.abc import Iterator
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any

import psycopg
import pytest
import schemathesis
from schemathesis import Case, GenerationMode
from schemathesis.core.parameters import ParameterLocation

ROOT = Path(__file__).resolve().parents[2]
OPENAPI = ROOT / "gen" / "openapi" / "fraud.yaml"
SERVICE_TOKEN = "contract-service-token"

pytestmark = [pytest.mark.integration, pytest.mark.enable_socket]

# The role each console operation needs (internal/auth/rbac.go).
OPERATION_PERSONA = {
    "CaseService_LabelCase": "analyst-1",
    "CaseService_TestRule": "risk-1",
    "CaseService_GetShadowSummary": "ml-1",
}


def _env(name: str) -> str:
    value = os.environ.get(name)
    if not value:
        raise RuntimeError(f"{name} is not set; run through `make test` (compose.test.yaml)")
    return value


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return int(s.getsockname()[1])


def _wait_http(url: str, deadline_s: float = 30.0) -> None:
    deadline = time.monotonic() + deadline_s
    last: Exception | None = None
    while time.monotonic() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=1) as resp:
                if resp.status == 200:
                    return
        except (urllib.error.URLError, OSError) as exc:
            last = exc
        time.sleep(0.1)
    raise TimeoutError(f"{url} not ready: {last}")


def _post(url: str, body: dict[str, Any], token: str) -> dict[str, Any]:
    req = urllib.request.Request(
        url,
        data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {token}"},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=5) as resp:
        result: dict[str, Any] = json.load(resp)
        return result


@pytest.fixture(scope="module")
def stack(tmp_path_factory: pytest.TempPathFactory) -> Iterator[dict[str, Any]]:
    admin_dsn = _env("FRAUD_TEST_PG_DSN")
    redis_addr = _env("FRAUD_TEST_REDIS_ADDR")
    brokers = _env("FRAUD_TEST_KAFKA_BROKERS")
    work = tmp_path_factory.mktemp("contract")
    env = {**os.environ, "GOTOOLCHAIN": "auto"}
    for name in ("scorer", "dev-issuer"):
        subprocess.run(
            ["go", "build", "-o", str(work / name), f"./cmd/{name}"], cwd=ROOT, env=env, check=True
        )

    db = "c_" + uuid.uuid4().hex[:12]
    with psycopg.connect(admin_dsn, autocommit=True) as conn:
        conn.execute(f"CREATE DATABASE {db}")  # db is a generated identifier
    base = admin_dsn.rsplit("/", 1)[0]
    owner_dsn = f"{base}/{db}?sslmode=disable"
    app_dsn = owner_dsn.replace("fraud:fraud@", "fraud_app:fraud_app@", 1)
    subprocess.run(
        [str(work / "scorer"), "migrate"], env={**env, "FRAUD_PG_MIGRATE_DSN": owner_dsn}, check=True
    )

    issuer_port, grpc_port, http_port = _free_port(), _free_port(), _free_port()
    issuer = f"http://127.0.0.1:{issuer_port}"
    procs = [
        subprocess.Popen(
            [str(work / "dev-issuer")],
            env={**env, "FRAUD_ISSUER": issuer, "FRAUD_ISSUER_ADDR": f"127.0.0.1:{issuer_port}"},
        )
    ]
    try:
        _wait_http(f"{issuer}/healthz")
        procs.append(
            subprocess.Popen(
                [str(work / "scorer")],
                cwd=ROOT,
                stdout=subprocess.DEVNULL,
                env={
                    **env,
                    "FRAUD_SERVICE_TOKEN": SERVICE_TOKEN,
                    "FRAUD_GRPC_ADDR": f"127.0.0.1:{grpc_port}",
                    "FRAUD_HTTP_ADDR": f"127.0.0.1:{http_port}",
                    "FRAUD_REDIS_ADDR": redis_addr,
                    "FRAUD_REDIS_PREFIX": db,  # isolates idempotency and feature keys per run
                    "FRAUD_PG_DSN": app_dsn,
                    "FRAUD_KAFKA_BROKERS": brokers,
                    "FRAUD_MODEL_URL": f"http://127.0.0.1:{_free_port()}",  # nothing listens: rules-only
                    "FRAUD_SPOOL_DIR": str(work / "spool"),
                    "FRAUD_ISSUER": issuer,
                    "FRAUD_JWKS_URL": f"{issuer}/.well-known/jwks.json",
                    "FRAUD_RATE_LIMIT_RPS": "0",  # the 20 req/s limit has its own Go test
                    "FRAUD_RULES_DIR": "rules",
                    "FRAUD_FEATURE_SPEC": "features/spec.yaml",
                },
            )
        )
        base_url = f"http://127.0.0.1:{http_port}"
        _wait_http(f"{base_url}/readyz")
        tokens = {
            p: _post(f"{issuer}/token", {"persona": p}, "")["access_token"]
            for p in ("analyst-1", "risk-1", "ml-1")
        }
        # Seed cases: high amounts are REVIEW while the model is unavailable.
        for i in range(5):
            tx = {
                "transaction_id": str(uuid.uuid4()),
                "account_id": f"acc_{i:08d}",
                "amount_minor": 90_000 + i,
                "currency": "USD",
                "merchant_id": "m_00042",
                "mcc": "5732",
                "channel": "CHANNEL_ECOMMERCE",
                "country": "US",
                "ip_prefix": "203.0.113.0/24",
                "device_id": "ab" * 32,
                # Within Score's accepted window (D-041): a minute ago.
                "event_ts": (datetime.now(UTC) - timedelta(minutes=1)).strftime("%Y-%m-%dT%H:%M:%SZ"),
            }
            _post(f"{base_url}/v1/score", {"transaction": tx}, SERVICE_TOKEN)
        yield {"base_url": base_url, "tokens": tokens}
    finally:
        for p in reversed(procs):
            p.terminate()
            try:
                p.wait(timeout=10)
            except subprocess.TimeoutExpired:
                p.kill()
        with psycopg.connect(admin_dsn, autocommit=True) as conn:
            conn.execute(f"DROP DATABASE IF EXISTS {db} WITH (FORCE)")


@pytest.fixture(scope="module")
def api_schema(stack: dict[str, Any]) -> schemathesis.BaseSchema:
    schema = schemathesis.openapi.from_path(OPENAPI)
    schema.config.update(base_url=stack["base_url"])
    return schema


# Server streaming (newline-delimited JSON) is outside what OpenAPI 3.0 can describe; Go tests cover it.
schema = schemathesis.pytest.from_fixture("api_schema").exclude(path="/v1/metrics/live")


@schema.parametrize()
def test_api_conforms_to_openapi(case: Case, stack: dict[str, Any]) -> None:
    op = case.operation.definition.raw.get("operationId", "")
    token = SERVICE_TOKEN if op == "Scorer_Score" else stack["tokens"][OPERATION_PERSONA.get(op, "analyst-1")]
    body = case.body if isinstance(case.body, dict) else None
    tx = body.get("transaction") if body else None
    if isinstance(tx, dict) and isinstance(tx.get("event_ts"), str):
        # The accepted window ([now - 24 h, now + 5 min], D-041) depends on the clock, which OpenAPI cannot
        # express (the field description states it); keep event_ts inside it so the rest is still checked.
        tx["event_ts"] = (datetime.now(UTC) - timedelta(minutes=1)).strftime("%Y-%m-%dT%H:%M:%SZ")
    if case.query:
        # page_token is opaque (issued by the server); arbitrary strings are a correct 400, and the token
        # round trip is covered by internal/store TestListCases* (see docs/DECISIONS.md D-020).
        case.query.pop("page_token", None)
    headers = {}
    header_info = case.meta.components.get(ParameterLocation.HEADER) if case.meta else None
    # A negative case may drop the Authorization header on purpose; only authenticate the others.
    if header_info is None or header_info.mode is GenerationMode.POSITIVE:
        headers["Authorization"] = f"Bearer {token}"
    case.call_and_validate(headers=headers)
