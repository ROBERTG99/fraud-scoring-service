"""Dev-issuer JWT verification (EdDSA, JWKS), mirroring internal/auth/jwt.go."""

from __future__ import annotations

import asyncio
import math
import os
import time
from collections.abc import Callable
from dataclasses import dataclass
from typing import Any, Protocol
from urllib.parse import urlsplit

import httpx
import jwt

AUDIENCE = "fraud-console"
CONSOLE_ROLES = frozenset({"analyst", "risk_engineer", "ml_engineer"})


def check_jwks_url(url: str) -> None:
    """Require HTTPS for the JWKS endpoint unless FRAUD_ENV=dev, as internal/auth.CheckJWKSURL (audit M6 M4).

    Keys fetched over plain HTTP could be replaced in transit, and with them every console token.
    """
    scheme = urlsplit(url).scheme
    if scheme == "https" or (scheme == "http" and os.environ.get("FRAUD_ENV") == "dev"):
        return
    if scheme == "http":
        raise ValueError(f"JWKS URL {url}: plain HTTP is allowed only with FRAUD_ENV=dev")
    raise ValueError(f"JWKS URL {url}: scheme must be https")


class InvalidTokenError(Exception):
    """The bearer token failed verification."""


@dataclass(frozen=True)
class Principal:
    subject: str
    role: str


class KeySource(Protocol):
    async def key(self, kid: str) -> Any: ...


class StaticKeys:
    """A fixed JWKS document (tests, or a pinned key set)."""

    def __init__(self, jwks: dict[str, Any]) -> None:
        self.keys = {k["kid"]: jwt.PyJWK(k).key for k in jwks.get("keys", [])}

    async def key(self, kid: str) -> Any:
        if kid not in self.keys:
            raise InvalidTokenError(f"unknown kid {kid!r}")
        return self.keys[kid]


JWKS_FRESH_S = 600.0  # a known kid is served without a refetch
JWKS_MIN_INTERVAL_S = 10.0  # at most one fetch attempt per interval, failed or not
JWKS_MAX_STALE_S = 3600.0  # keys this old are not used even when the issuer is down


class RemoteKeys:
    """Fetches and caches a JWKS URL, as internal/auth.RemoteKeys (audit M6 L3).

    One fetch runs at a time and the lock is not held during it: callers with a usable cached key are served
    at once, the others await that fetch. Attempts are at most one per 10 s, also after a failure, and cached
    keys are accepted for at most an hour after the last good fetch.
    """

    def __init__(
        self,
        url: str,
        *,
        timeout_s: float = 2.0,
        transport: httpx.AsyncBaseTransport | None = None,
        clock: Callable[[], float] = time.monotonic,
    ) -> None:
        self.url = url
        self.timeout_s = timeout_s
        self.transport = transport
        self.clock = clock
        self._keys: dict[str, Any] = {}
        self._fetched: float | None = None
        self._tried: float | None = None
        self._last_error: Exception | None = None
        self._inflight: asyncio.Future[None] | None = None

    async def key(self, kid: str) -> Any:
        while True:
            now = self.clock()
            age = now - self._fetched if self._fetched is not None else math.inf
            known = kid in self._keys
            usable = known and age < JWKS_MAX_STALE_S
            if known and age < JWKS_FRESH_S:
                return self._keys[kid]
            if self._inflight is not None:
                if usable:
                    return self._keys[kid]
                await asyncio.shield(self._inflight)
                continue
            if self._tried is not None and now - self._tried < JWKS_MIN_INTERVAL_S:
                if usable:
                    return self._keys[kid]
                if not self._keys or known:
                    raise InvalidTokenError(f"jwks unavailable: {self._last_error}")
                raise InvalidTokenError(f"unknown kid {kid!r}")
            self._tried = now
            done: asyncio.Future[None] = asyncio.get_running_loop().create_future()
            self._inflight = done
            try:
                self._keys, self._fetched = await self._fetch(), self.clock()
                self._last_error = None
            except (httpx.HTTPError, ValueError, KeyError) as exc:
                self._last_error = exc
            finally:
                self._inflight = None
                done.set_result(None)
            # Loop once more: the new keys, or (tried is now recent) the stale key or an error.

    async def _fetch(self) -> dict[str, Any]:
        async with httpx.AsyncClient(timeout=self.timeout_s, transport=self.transport) as client:
            resp = await client.get(self.url)
            resp.raise_for_status()
            return StaticKeys(resp.json()).keys


class Verifier:
    def __init__(self, issuer: str, keys: KeySource) -> None:
        self.issuer = issuer
        self.keys = keys

    async def verify(self, token: str) -> Principal:
        try:
            header = jwt.get_unverified_header(token)
            if header.get("alg") != "EdDSA":
                raise InvalidTokenError("algorithm must be EdDSA")
            key = await self.keys.key(str(header.get("kid", "")))
            claims = jwt.decode(
                token,
                key=key,
                algorithms=["EdDSA"],
                audience=AUDIENCE,
                issuer=self.issuer,
                leeway=5,
                options={"require": ["exp", "sub", "iss", "aud"]},
            )
        except jwt.PyJWTError as exc:
            raise InvalidTokenError(str(exc)) from exc
        role = str(claims.get("role", ""))
        if role not in CONSOLE_ROLES:
            raise InvalidTokenError(f"unknown role {role!r}")
        return Principal(subject=str(claims["sub"]), role=role)
