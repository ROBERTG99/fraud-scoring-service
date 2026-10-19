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
from collections.abc import AsyncIterator, Awaitable, Callable
from contextlib import asynccontextmanager
from dataclasses import dataclass, field
from typing import Annotated, Any, Literal, Protocol
from wsgiref.simple_server import WSGIServer

from fastapi import BackgroundTasks, Depends, FastAPI, Header, HTTPException, Request
from prometheus_client import start_http_server
from pydantic import BaseModel, ConfigDict

from fraud_summarizer.auth import (
    InvalidTokenError,
    Principal,
    RemoteKeys,
    Verifier,
    check_database_dsn,
    check_jwks_url,
)
from fraud_summarizer.consumer import DecisionHandler, KafkaDecisionConsumer
from fraud_summarizer.llm import routes
from fraud_summarizer.llm.factory import build_gateway
from fraud_summarizer.pipeline import SummaryPipeline
from fraud_summarizer.telemetry import configure_logging, init_tracing

log = logging.getLogger(__name__)

REGENERATE_ROLES = frozenset({"analyst", "risk_engineer"})
_UUID = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")
_IDEM_KEY = re.compile(r"^[A-Za-z0-9_-]{8,128}$")


class SummaryStore(Protocol):
    async def ping(self) -> None: ...

    async def get_summary(self, case_id: str) -> dict[str, Any] | None: ...

    async def decision_for_case(self, case_id: str) -> str | None: ...

    async def record_regeneration(
        self, subject: str, case_id: str, key: str, *, limit: int, window_s: float
    ) -> str:
        """Record a regenerate request unless it is a replay ("duplicate") or subject already made `limit`
        requests in the trailing window ("limited"); otherwise "accepted". Shared by every process."""
        ...


class TokenVerifier(Protocol):
    async def verify(self, token: str) -> Principal: ...


class RegenerateAccepted(BaseModel):
    """202 response of POST /v1/summaries/{case_id}:regenerate."""

    model_config = ConfigDict(extra="forbid")

    case_id: str
    status: Literal["accepted"]
    run_id: str | None = None
    duplicate: bool = False


class TokenBucket:
    """`rate` requests per second per subject, burst `rate` (in memory, per process, as the scorer's limiter)."""

    MAX_SUBJECTS = 10_000  # bounds memory; buckets refill anyway when the map is reset

    def __init__(self, rate: float, clock: Callable[[], float] = time.monotonic) -> None:
        self.rate = rate
        self.clock = clock
        self._buckets: dict[str, tuple[float, float]] = {}  # subject -> (tokens, last)

    def allow(self, subject: str) -> bool:
        if self.rate <= 0:
            return True
        now = self.clock()
        if subject not in self._buckets and len(self._buckets) >= self.MAX_SUBJECTS:
            self._buckets.clear()
        tokens, last = self._buckets.get(subject, (self.rate, now))
        tokens = min(self.rate, tokens + (now - last) * self.rate)
        if tokens < 1:
            self._buckets[subject] = (tokens, now)
            return False
        self._buckets[subject] = (tokens - 1, now)
        return True


@dataclass
class Services:
    store: SummaryStore
    pipeline: SummaryPipeline
    verifier: TokenVerifier
    background: list[asyncio.Task[Any]] = field(default_factory=list)
    # Paid regenerations per user per hour, counted in Postgres (NFR-9, audit M6 L6).
    regenerate_limit: int = 20
    regenerate_window_s: float = 3600.0
    request_limit: TokenBucket = field(
        default_factory=lambda: TokenBucket(20)
    )  # spec §4.4: 20 req/s per user


def start_admin_metrics(addr: str) -> WSGIServer:
    """Serve /metrics on addr ("host:port"), apart from the API port (audit M6 L2)."""
    host, _, port = addr.rpartition(":")
    server, _thread = start_http_server(int(port), host or "127.0.0.1")
    return server


Factory = Callable[[], Awaitable[tuple[Services, Callable[[], Awaitable[None]]]]]


async def _production_services() -> tuple[Services, Callable[[], Awaitable[None]]]:
    configure_logging("case-summarizer")  # production only: tests keep pytest's log capture
    from fraud_summarizer.repository import PostgresRepository

    issuer = os.environ.get("FRAUD_ISSUER", "http://127.0.0.1:8515")
    jwks = os.environ.get("FRAUD_JWKS_URL", f"{issuer}/.well-known/jwks.json")
    check_jwks_url(jwks)
    dsn = os.environ.get(
        "FRAUD_SUMMARIZER_PG_DSN", "postgresql://fraud_summarizer:fraud_summarizer@127.0.0.1:8520/fraud"
    )
    check_database_dsn(dsn)
    admin = start_admin_metrics(os.environ.get("FRAUD_SUMMARIZER_ADMIN_ADDR", "127.0.0.1:8517"))
    pool = PostgresRepository.create_pool(dsn)
    await pool.open(wait=False)
    repo = PostgresRepository(pool)
    pipeline = SummaryPipeline(build_gateway(routes.load()), repo)
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
        admin.shutdown()
        admin.server_close()

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
        who = await svc.verifier.verify(auth.removeprefix("Bearer ").strip())
    except InvalidTokenError as exc:
        raise HTTPException(401, "invalid token") from exc
    if not svc.request_limit.allow(who.subject):
        raise HTTPException(429, "rate limit exceeded")
    return who


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
        shutdown_tracing = init_tracing("case-summarizer")
        services, close = await factory()
        for task in services.background:
            task.add_done_callback(_report_stopped)
        app.state.services = services
        try:
            yield
        finally:
            await close()
            shutdown_tracing()

    # No /docs, /redoc or /openapi.json: the document is generated offline (scripts/summarizer_openapi.py).
    app = FastAPI(
        title="fraud case-summarizer",
        version="0.1.0",
        lifespan=lifespan,
        docs_url=None,
        redoc_url=None,
        openapi_url=None,
    )

    @app.get("/healthz")
    async def healthz() -> dict[str, str]:
        return {"status": "ok"}

    @app.get("/readyz")
    async def readyz(svc: Annotated[Services, Depends(services)]) -> dict[str, str]:
        # The cause is logged, never returned: the public port must not describe the internals (D-060).
        try:
            await asyncio.wait_for(svc.store.ping(), timeout=1.0)
        except Exception as exc:
            log.warning("not ready: postgres %s", type(exc).__name__)
            raise HTTPException(503, "not ready") from exc
        for task in svc.background:
            if task.done():
                log.warning("not ready: %s stopped", task.get_name())
                raise HTTPException(503, "not ready")
        return {"status": "ready"}

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
        # Keys are scoped to the case: the same key on another case is a different request. Each
        # regeneration is a paid model call, bounded per user (NFR-9); a refused request records nothing,
        # so it can be retried with the same key.
        outcome = await svc.store.record_regeneration(
            who.subject,
            case_id,
            idempotency_key,
            limit=svc.regenerate_limit,
            window_s=svc.regenerate_window_s,
        )
        if outcome == "duplicate":
            return RegenerateAccepted(case_id=case_id, status="accepted", duplicate=True)
        if outcome == "limited":
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
