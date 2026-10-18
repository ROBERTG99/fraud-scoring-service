"""Summarizer HTTP API (spec §4.4): auth, RBAC, idempotent regenerate."""

from __future__ import annotations

import asyncio
import base64
import logging
import time
from collections.abc import Awaitable, Callable, Iterator
from dataclasses import dataclass, field
from typing import Any

import jwt
import pytest
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat
from fastapi.testclient import TestClient

from fraud_summarizer.app import Services, TokenBucket, create_app
from fraud_summarizer.auth import AUDIENCE, InvalidTokenError, StaticKeys, Verifier, check_jwks_url
from fraud_summarizer.llm import LLMGateway
from fraud_summarizer.llm.fake import FakeLLM
from fraud_summarizer.llm.routes import load
from fraud_summarizer.pipeline import SummaryPipeline
from tests.unit.test_pipeline import MemoryRepo, journey_case

ISSUER = "http://issuer.test"
CASE_ID = "11111111-1111-4111-8111-111111111111"
KEY = Ed25519PrivateKey.generate()


def jwks() -> dict[str, Any]:
    raw = KEY.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)
    x = base64.urlsafe_b64encode(raw).rstrip(b"=").decode()
    return {"keys": [{"kty": "OKP", "crv": "Ed25519", "x": x, "kid": "k1", "alg": "EdDSA", "use": "sig"}]}


def token(
    role: str, *, sub: str = "user-1", exp: float | None = None, aud: str = AUDIENCE, kid: str = "k1"
) -> str:
    claims = {"sub": sub, "role": role, "iss": ISSUER, "aud": aud, "exp": int(exp or time.time() + 3600)}
    return jwt.encode(claims, KEY, algorithm="EdDSA", headers={"kid": kid})


@dataclass
class Store(MemoryRepo):
    processed: set[tuple[str, str]] = field(default_factory=set)

    async def ping(self) -> None:
        return None

    async def get_summary(self, case_id: str) -> dict[str, Any] | None:
        for r in reversed(self.saved):
            if r.case_id == case_id:
                return {"case_id": case_id, "status": r.status, "summary": r.summary.model_dump(mode="json")}
        return None

    async def decision_for_case(self, case_id: str) -> str | None:
        return next((d for d, c in self.cases.items() if c.case_id == case_id), None)

    async def is_processed(self, consumer: str, key: str) -> bool:
        return (consumer, key) in self.processed

    async def mark_processed(self, consumer: str, key: str) -> bool:
        if (consumer, key) in self.processed:
            return False
        self.processed.add((consumer, key))
        return True

    regenerations: dict[tuple[str, str], tuple[str, float]] = field(default_factory=dict)

    async def record_regeneration(
        self, subject: str, case_id: str, key: str, *, limit: int, window_s: float
    ) -> str:
        if (case_id, key) in self.regenerations:
            return "duplicate"
        now = time.monotonic()
        recent = [t for s, t in self.regenerations.values() if s == subject and t > now - window_s]
        if len(recent) >= limit:
            return "limited"
        self.regenerations[(case_id, key)] = (subject, now)
        return "accepted"


@pytest.fixture
def store() -> Store:
    return Store({"d1": journey_case()})


@pytest.fixture
def client(store: Store) -> Iterator[TestClient]:
    async def factory() -> tuple[Services, Callable[[], Awaitable[None]]]:
        gw = LLMGateway(load(env={"FRAUD_SUMMARY_PROVIDER": "fake"}), {"fake": FakeLLM()})
        svc = Services(
            store=store, pipeline=SummaryPipeline(gw, store), verifier=Verifier(ISSUER, StaticKeys(jwks()))
        )

        async def close() -> None:
            return None

        return svc, close

    with TestClient(create_app(factory)) as c:
        yield c


def auth(role: str) -> dict[str, str]:
    return {"Authorization": f"Bearer {token(role)}"}


def test_get_summary_requires_valid_token(client: TestClient) -> None:
    assert client.get(f"/v1/summaries/{CASE_ID}").status_code == 401
    assert client.get(f"/v1/summaries/{CASE_ID}", headers={"Authorization": "Bearer nope"}).status_code == 401
    expired = {"Authorization": f"Bearer {token('analyst', exp=time.time() - 60)}"}
    assert client.get(f"/v1/summaries/{CASE_ID}", headers=expired).status_code == 401
    wrong_aud = {"Authorization": f"Bearer {token('analyst', aud='other')}"}
    assert client.get(f"/v1/summaries/{CASE_ID}", headers=wrong_aud).status_code == 401
    unknown_role = {"Authorization": f"Bearer {token('admin')}"}
    assert client.get(f"/v1/summaries/{CASE_ID}", headers=unknown_role).status_code == 401
    assert client.get(f"/v1/summaries/{CASE_ID}", headers=auth("ml_engineer")).status_code == 404
    assert client.get("/v1/summaries/not-a-uuid", headers=auth("analyst")).status_code == 400


def test_regenerate_is_idempotent_and_role_checked(client: TestClient, store: Store) -> None:
    url = f"/v1/summaries/{CASE_ID}:regenerate"
    key = {"Idempotency-Key": "regen-key-0001"}
    assert client.post(url, headers={**auth("ml_engineer"), **key}).status_code == 403
    assert client.post(url, headers=auth("analyst")).status_code == 400  # no Idempotency-Key
    assert client.post(url, headers={**auth("analyst"), "Idempotency-Key": "short"}).status_code == 400
    missing = "22222222-2222-4222-8222-222222222222"
    assert (
        client.post(f"/v1/summaries/{missing}:regenerate", headers={**auth("analyst"), **key}).status_code
        == 404
    )

    first = client.post(url, headers={**auth("risk_engineer"), **key})
    assert first.status_code == 202 and "run_id" in first.json()
    assert len(store.saved) == 1  # the background task ran after the response
    again = client.post(url, headers={**auth("analyst"), **key})
    assert again.status_code == 202 and again.json()["duplicate"] is True
    assert len(store.saved) == 1

    got = client.get(f"/v1/summaries/{CASE_ID}", headers=auth("analyst"))
    assert got.status_code == 200 and got.json()["status"] == "ok"


def test_health_ready_and_no_public_metrics_or_docs(client: TestClient) -> None:
    """Audit M6 L2: metrics are on the admin listener; the API document is generated offline."""
    assert client.get("/healthz").json() == {"status": "ok"}
    assert client.get("/readyz").json() == {"status": "ready"}
    for path in ("/metrics", "/docs", "/redoc", "/openapi.json"):
        assert client.get(path).status_code == 404, path


def test_metrics_are_served_on_the_admin_listener() -> None:
    import urllib.request

    from fraud_summarizer.app import start_admin_metrics

    server = start_admin_metrics("127.0.0.1:0")
    try:
        port = server.server_port
        with urllib.request.urlopen(f"http://127.0.0.1:{port}/metrics", timeout=5) as resp:
            assert "fraud_summary_status_total" in resp.read().decode()
    finally:
        server.shutdown()
        server.server_close()


def test_console_requests_are_limited_per_user(client: TestClient) -> None:
    """Audit M6 L6: the scorer's 20 req/s per user applies to the summarizer's API too."""
    svc = client.app.state.services  # type: ignore[attr-defined]
    svc.request_limit = TokenBucket(2)
    url = f"/v1/summaries/{CASE_ID}"
    assert [client.get(url, headers=auth("analyst")).status_code for _ in range(3)] == [404, 404, 429]
    other = {"Authorization": f"Bearer {token('analyst', sub='analyst-2')}"}
    assert client.get(url, headers=other).status_code == 404  # another user has their own bucket


def test_token_bucket_refills() -> None:
    now = [0.0]
    b = TokenBucket(2, clock=lambda: now[0])
    assert b.allow("a") and b.allow("a") and not b.allow("a")
    now[0] = 0.5  # one token back at 2/s
    assert b.allow("a") and not b.allow("a")


async def test_verifier_rejects_other_algorithms() -> None:
    v = Verifier(ISSUER, StaticKeys(jwks()))
    hs = jwt.encode(
        {"sub": "x", "role": "analyst", "iss": ISSUER, "aud": AUDIENCE, "exp": time.time() + 60}, "k" * 32
    )
    with pytest.raises(InvalidTokenError):
        await v.verify(hs)
    with pytest.raises(InvalidTokenError):
        await v.verify(token("analyst", kid="other"))
    p = await v.verify(token("risk_engineer", sub="risk-1"))
    assert (p.subject, p.role) == ("risk-1", "risk_engineer")


async def test_remote_keys_fetch_cache_and_failure() -> None:
    import httpx

    from fraud_summarizer.auth import RemoteKeys

    calls = 0
    up = True

    def handler(request: httpx.Request) -> httpx.Response:
        nonlocal calls
        calls += 1
        if not up:
            return httpx.Response(503)
        return httpx.Response(200, json=jwks())

    keys = RemoteKeys("http://issuer.test/.well-known/jwks.json", transport=httpx.MockTransport(handler))
    v = Verifier(ISSUER, keys)
    assert (await v.verify(token("analyst"))).role == "analyst"
    assert (await v.verify(token("analyst"))).role == "analyst"
    assert calls == 1  # cached
    with pytest.raises(InvalidTokenError):
        await v.verify(token("analyst", kid="rotated"))  # refresh is rate limited to one per 10 s
    assert calls == 1

    up = False
    down = RemoteKeys("http://issuer.test/jwks", transport=httpx.MockTransport(handler))
    with pytest.raises(InvalidTokenError, match="jwks unavailable"):
        await down.key("k1")


async def test_missing_sdk_credentials_degrade_to_fallback(monkeypatch: pytest.MonkeyPatch) -> None:
    from fraud_summarizer.llm.factory import UnavailableProvider, build_gateway
    from fraud_summarizer.llm.types import ProviderError

    monkeypatch.delenv("OPENAI_API_KEY", raising=False)
    gw = build_gateway(load(env={"FRAUD_SUMMARY_PROVIDER": "openai"}))
    provider = gw.providers["openai"]
    assert isinstance(provider, UnavailableProvider)
    with pytest.raises(ProviderError) as exc:
        await provider.complete(None)  # type: ignore[arg-type]
    assert exc.value.kind == "unavailable" and not exc.value.retryable


def test_regenerate_keys_are_per_case_and_rate_limited(client: TestClient, store: Store) -> None:
    other = "33333333-3333-4333-8333-333333333333"
    store.cases["d2"] = journey_case()
    store.cases["d2"].case_id = other
    key = {"Idempotency-Key": "shared-key-0001"}
    assert (
        client.post(f"/v1/summaries/{CASE_ID}:regenerate", headers={**auth("analyst"), **key}).json()[
            "duplicate"
        ]
        is False
    )
    # The same key on a different case is a different request, not a replay.
    second = client.post(f"/v1/summaries/{other}:regenerate", headers={**auth("analyst"), **key})
    assert second.status_code == 202 and second.json()["duplicate"] is False

    svc = client.app.state.services  # type: ignore[attr-defined]
    svc.regenerate_limit = 2
    third = client.post(
        f"/v1/summaries/{CASE_ID}:regenerate",
        headers={**auth("analyst"), "Idempotency-Key": "fresh-key-0003"},
    )
    assert third.status_code == 429


def test_a_crashed_background_consumer_is_logged_and_fails_readiness(
    store: Store, caplog: pytest.LogCaptureFixture
) -> None:
    async def crash() -> None:
        raise RuntimeError("Libraries for snappy compression codec not found")

    async def factory() -> tuple[Services, Callable[[], Awaitable[None]]]:
        gw = LLMGateway(load(env={"FRAUD_SUMMARY_PROVIDER": "fake"}), {"fake": FakeLLM()})
        svc = Services(
            store=store, pipeline=SummaryPipeline(gw, store), verifier=Verifier(ISSUER, StaticKeys(jwks()))
        )
        svc.background.append(asyncio.create_task(crash(), name="decisions-consumer"))

        async def close() -> None:
            return None

        return svc, close

    with caplog.at_level(logging.ERROR, logger="fraud_summarizer.app"), TestClient(create_app(factory)) as c:
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline and not caplog.records:
            c.get("/healthz")  # lets the event loop run the task and its callback
        assert c.get("/readyz").status_code == 503
    crashed = [r for r in caplog.records if "decisions-consumer" in r.getMessage()]
    assert crashed and crashed[0].exc_info is not None, "the crash must be logged with its traceback"


def test_a_rate_limited_regenerate_does_not_consume_its_key(client: TestClient) -> None:
    svc = client.app.state.services  # type: ignore[attr-defined]
    svc.regenerate_limit = 0
    headers = {**auth("analyst"), "Idempotency-Key": "retry-after-429"}
    assert client.post(f"/v1/summaries/{CASE_ID}:regenerate", headers=headers).status_code == 429
    # The refused request never ran: a retry with the same key once the limit allows is a new request.
    svc.regenerate_limit = 20
    retry = client.post(f"/v1/summaries/{CASE_ID}:regenerate", headers=headers)
    assert retry.status_code == 202
    assert retry.json()["duplicate"] is False and retry.json()["run_id"]
    again = client.post(f"/v1/summaries/{CASE_ID}:regenerate", headers=headers)
    assert again.json()["duplicate"] is True


def test_jwks_url_needs_https_outside_dev(monkeypatch: pytest.MonkeyPatch) -> None:
    """Keys fetched over plain HTTP could be swapped in transit (audit M6 M4, D-045)."""
    monkeypatch.delenv("FRAUD_ENV", raising=False)
    check_jwks_url("https://issuer.example/.well-known/jwks.json")
    for bad in ("http://127.0.0.1:8515/.well-known/jwks.json", "file:///etc/jwks.json", "issuer/jwks"):
        with pytest.raises(ValueError, match="JWKS URL"):
            check_jwks_url(bad)
    monkeypatch.setenv("FRAUD_ENV", "dev")
    check_jwks_url("http://127.0.0.1:8515/.well-known/jwks.json")
    with pytest.raises(ValueError, match="JWKS URL"):
        check_jwks_url("ftp://127.0.0.1/jwks.json")


async def test_remote_keys_single_flight_backoff_and_stale_cap() -> None:
    """Audit M6 L3, as internal/auth TestRemoteKeysSingleFlightBackoffAndStaleCap."""
    import httpx

    from fraud_summarizer.auth import RemoteKeys

    calls = 0
    up = True
    gate = asyncio.Event()

    async def handler(request: httpx.Request) -> httpx.Response:
        nonlocal calls
        calls += 1
        await gate.wait()
        return httpx.Response(200, json=jwks()) if up else httpx.Response(503)

    now = 1_000.0
    keys = RemoteKeys("https://issuer.test/jwks", transport=httpx.MockTransport(handler), clock=lambda: now)
    waiting = [asyncio.create_task(keys.key("k1")) for _ in range(20)]
    await asyncio.sleep(0.05)
    assert calls == 1, "20 concurrent misses share one fetch"
    gate.set()
    await asyncio.gather(*waiting)

    up = False
    now += 11 * 60  # past fresh: the refresh fails, the stale key is served
    await keys.key("k1")
    await keys.key("k1")
    assert calls == 2, "no second attempt within 10 s of a failure"
    with pytest.raises(InvalidTokenError):
        await keys.key("rotated")
    assert calls == 2
    now += 50 * 60  # an hour after the last good fetch
    with pytest.raises(InvalidTokenError, match="jwks unavailable"):
        await keys.key("k1")
    up = True
    now += 11
    await keys.key("k1")


async def test_remote_keys_serve_a_known_key_during_a_slow_refresh() -> None:
    import httpx

    from fraud_summarizer.auth import RemoteKeys

    slow = False
    gate = asyncio.Event()

    async def handler(request: httpx.Request) -> httpx.Response:
        if slow:
            await gate.wait()
        return httpx.Response(200, json=jwks())

    now = 1_000.0
    keys = RemoteKeys("https://issuer.test/jwks", transport=httpx.MockTransport(handler), clock=lambda: now)
    await keys.key("k1")
    slow, now = True, now + 11 * 60
    refresh = asyncio.create_task(keys.key("k1"))
    await asyncio.sleep(0.05)
    await asyncio.wait_for(keys.key("k1"), timeout=1)  # not behind the refresh
    gate.set()
    await refresh
