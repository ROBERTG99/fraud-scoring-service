"""Summarizer HTTP API (spec §4.4): auth, RBAC, idempotent regenerate."""

from __future__ import annotations

import base64
import time
from collections.abc import Awaitable, Callable, Iterator
from dataclasses import dataclass, field
from typing import Any

import jwt
import pytest
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat
from fastapi.testclient import TestClient

from fraud_summarizer.app import Services, create_app
from fraud_summarizer.auth import AUDIENCE, InvalidTokenError, StaticKeys, Verifier
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

    async def mark_processed(self, consumer: str, key: str) -> bool:
        if (consumer, key) in self.processed:
            return False
        self.processed.add((consumer, key))
        return True


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


def test_health_ready_metrics(client: TestClient) -> None:
    assert client.get("/healthz").json() == {"status": "ok"}
    assert client.get("/readyz").json() == {"status": "ready"}
    assert "fraud_summary_status_total" in client.get("/metrics").text


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
