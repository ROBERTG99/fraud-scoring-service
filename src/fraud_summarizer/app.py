"""FastAPI application for the case summarizer (port 8513).

Runs the `decisions` consumer in the background and serves:
  GET  /v1/summaries/{case_id}              any console role
  POST /v1/summaries/{case_id}:regenerate   analyst or risk_engineer, `Idempotency-Key` header, 202
"""

from __future__ import annotations

import asyncio
import contextlib
import logging
import os
import re
import time
import uuid
from collections import deque
from collections.abc import AsyncIterator, Awaitable, Callable
from contextlib import asynccontextmanager
from dataclasses import dataclass, field
from typing import Annotated, Any, Literal, Protocol

from fastapi import BackgroundTasks, Depends, FastAPI, Header, HTTPException, Request
from fastapi.responses import Response
from prometheus_client import CONTENT_TYPE_LATEST, generate_latest
from pydantic import BaseModel, ConfigDict

from fraud_summarizer.auth import InvalidTokenError, Principal, RemoteKeys, Verifier
from fraud_summarizer.consumer import DecisionHandler, KafkaDecisionConsumer
from fraud_summarizer.llm import routes
from fraud_summarizer.llm.factory import build_gateway
from fraud_summarizer.pipeline import SummaryPipeline

log = logging.getLogger(__name__)

REGENERATE_CONSUMER = "regenerate"
REGENERATE_ROLES = frozenset({"analyst", "risk_engineer"})
_UUID = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")
_IDEM_KEY = re.compile(r"^[A-Za-z0-9_-]{8,128}$")


class SummaryStore(Protocol):
    async def ping(self) -> None: ...

    async def get_summary(self, case_id: str) -> dict[str, Any] | None: ...

    async def decision_for_case(self, case_id: str) -> str | None: ...

    async def mark_processed(self, consumer: str, key: str) -> bool: ...


class TokenVerifier(Protocol):
    async def verify(self, token: str) -> Principal: ...


class RegenerateAccepted(BaseModel):
    """202 response of POST /v1/summaries/{case_id}:regenerate."""

    model_config = ConfigDict(extra="forbid")

    case_id: str
    status: Literal["accepted"]
    run_id: str | None = None
    duplicate: bool = False


class SlidingWindowLimit:
    """At most `limit` events per subject in the trailing `window_s` seconds (in memory, per process)."""

    def __init__(self, limit: int, window_s: float, clock: Callable[[], float] = time.monotonic) -> None:
        self.limit = limit
        self.window_s = window_s
        self.clock = clock
        self._hits: dict[str, deque[float]] = {}

    def allow(self, subject: str) -> bool:
        now = self.clock()
        q = self._hits.setdefault(subject, deque())
        while q and q[0] <= now - self.window_s:
            q.popleft()
        if len(q) >= self.limit:
            return False
        q.append(now)
        return True


@dataclass
class Services:
    store: SummaryStore
    pipeline: SummaryPipeline
    verifier: TokenVerifier
    background: list[asyncio.Task[Any]] = field(default_factory=list)
    regenerate_limit: SlidingWindowLimit = field(default_factory=lambda: SlidingWindowLimit(20, 3600))


Factory = Callable[[], Awaitable[tuple[Services, Callable[[], Awaitable[None]]]]]


async def _production_services() -> tuple[Services, Callable[[], Awaitable[None]]]:
    from fraud_summarizer.repository import PostgresRepository

    dsn = os.environ.get("FRAUD_SUMMARIZER_PG_DSN", "postgresql://fraud_app:fraud_app@127.0.0.1:8520/fraud")
    pool = PostgresRepository.create_pool(dsn)
    await pool.open(wait=False)
    repo = PostgresRepository(pool)
    pipeline = SummaryPipeline(build_gateway(routes.load()), repo)
    issuer = os.environ.get("FRAUD_ISSUER", "http://127.0.0.1:8515")
    jwks = os.environ.get("FRAUD_JWKS_URL", f"{issuer}/.well-known/jwks.json")
    services = Services(store=repo, pipeline=pipeline, verifier=Verifier(issuer, RemoteKeys(jwks)))
    stop = asyncio.Event()
    brokers = os.environ.get("FRAUD_KAFKA_BROKERS", "")
    if brokers:
        consumer = KafkaDecisionConsumer(DecisionHandler(pipeline, repo), brokers)
        services.background.append(asyncio.create_task(consumer.run(stop), name="decisions-consumer"))

    async def close() -> None:
        stop.set()
        for task in services.background:
            with contextlib.suppress(asyncio.CancelledError, Exception):
                await asyncio.wait_for(task, timeout=10)
        await pool.close()

    return services, close


def services(request: Request) -> Services:
    svc: Services | None = getattr(request.app.state, "services", None)
    if svc is None:
        raise HTTPException(503, "starting")
    return svc


async def principal(request: Request, svc: Annotated[Services, Depends(services)]) -> Principal:
    auth = request.headers.get("authorization", "")
    if not auth.startswith("Bearer "):
        raise HTTPException(401, "missing bearer token")
    try:
        return await svc.verifier.verify(auth.removeprefix("Bearer ").strip())
    except InvalidTokenError as exc:
        raise HTTPException(401, "invalid token") from exc


def _report_stopped(task: asyncio.Task[Any]) -> None:
    """A background loop never ends on its own before shutdown: log why it stopped (readyz turns 503)."""
    if task.cancelled():
        return
    exc = task.exception()
    if exc is not None:
        log.error("%s stopped", task.get_name(), exc_info=exc)


def create_app(factory: Factory | None = None) -> FastAPI:
    factory = factory or _production_services

    @asynccontextmanager
    async def lifespan(app: FastAPI) -> AsyncIterator[None]:
        services, close = await factory()
        for task in services.background:
            task.add_done_callback(_report_stopped)
        app.state.services = services
        try:
            yield
        finally:
            await close()

    app = FastAPI(title="fraud case-summarizer", version="0.1.0", lifespan=lifespan)

    @app.get("/healthz")
    async def healthz() -> dict[str, str]:
        return {"status": "ok"}

    @app.get("/readyz")
    async def readyz(svc: Annotated[Services, Depends(services)]) -> dict[str, str]:
        try:
            await asyncio.wait_for(svc.store.ping(), timeout=1.0)
        except Exception as exc:
            raise HTTPException(503, f"postgres: {type(exc).__name__}") from exc
        for task in svc.background:
            if task.done():
                raise HTTPException(503, f"{task.get_name()} stopped")
        return {"status": "ready"}

    @app.get("/metrics")
    async def metrics() -> Response:
        return Response(generate_latest(), media_type=CONTENT_TYPE_LATEST)

    @app.get("/v1/summaries/{case_id}")
    async def get_summary(
        case_id: str,
        svc: Annotated[Services, Depends(services)],
        _: Annotated[Principal, Depends(principal)],
    ) -> dict[str, Any]:
        if not _UUID.match(case_id):
            raise HTTPException(400, "case_id must be a UUID")
        summary = await svc.store.get_summary(case_id)
        if summary is None:
            raise HTTPException(404, "no summary yet")
        return summary

    @app.post("/v1/summaries/{case_id}:regenerate", status_code=202, response_model=RegenerateAccepted)
    async def regenerate(
        case_id: str,
        background: BackgroundTasks,
        svc: Annotated[Services, Depends(services)],
        who: Annotated[Principal, Depends(principal)],
        idempotency_key: Annotated[str | None, Header(alias="Idempotency-Key")] = None,
    ) -> RegenerateAccepted:
        if who.role not in REGENERATE_ROLES:
            raise HTTPException(403, f"role {who.role} may not regenerate summaries")
        if not _UUID.match(case_id):
            raise HTTPException(400, "case_id must be a UUID")
        if idempotency_key is None or not _IDEM_KEY.match(idempotency_key):
            raise HTTPException(400, "Idempotency-Key header: 8-128 characters of [A-Za-z0-9_-]")
        decision_id = await svc.store.decision_for_case(case_id)
        if decision_id is None:
            raise HTTPException(404, "case not found")
        # Keys are scoped to the case: the same key on another case is a different request.
        if not await svc.store.mark_processed(REGENERATE_CONSUMER, f"{case_id}:{idempotency_key}"):
            return RegenerateAccepted(case_id=case_id, status="accepted", duplicate=True)
        # Each regeneration is a paid model call: bound it per user (NFR-9).
        if not svc.regenerate_limit.allow(who.subject):
            raise HTTPException(429, "regeneration rate limit: try again later")
        run_id = str(uuid.uuid4())

        async def run() -> None:
            try:
                await svc.pipeline.run(decision_id, run_id=run_id, replace=True)
            except Exception:
                log.exception("regenerate failed", extra={"case_id": case_id, "run_id": run_id})

        background.add_task(run)
        log.info("summary regeneration requested", extra={"case_id": case_id, "actor": who.subject})
        return RegenerateAccepted(case_id=case_id, status="accepted", run_id=run_id)

    return app


app = create_app()
