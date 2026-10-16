"""Dev-issuer JWT verification (EdDSA, JWKS), mirroring internal/auth/jwt.go."""

from __future__ import annotations

import asyncio
import time
from dataclasses import dataclass
from typing import Any, Protocol

import httpx
import jwt

AUDIENCE = "fraud-console"
CONSOLE_ROLES = frozenset({"analyst", "risk_engineer", "ml_engineer"})


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


class RemoteKeys:
    """Fetches and caches a JWKS URL; an unknown kid triggers at most one refresh per 10 s."""

    def __init__(
        self, url: str, *, timeout_s: float = 2.0, transport: httpx.AsyncBaseTransport | None = None
    ) -> None:
        self.url = url
        self.timeout_s = timeout_s
        self.transport = transport
        self._keys: dict[str, Any] = {}
        self._fetched = 0.0
        self._lock = asyncio.Lock()

    async def key(self, kid: str) -> Any:
        async with self._lock:
            fresh = time.monotonic() - self._fetched < 600
            if kid in self._keys and fresh:
                return self._keys[kid]
            if not self._keys or time.monotonic() - self._fetched > 10:
                try:
                    async with httpx.AsyncClient(timeout=self.timeout_s, transport=self.transport) as client:
                        resp = await client.get(self.url)
                        resp.raise_for_status()
                        self._keys = StaticKeys(resp.json()).keys
                        self._fetched = time.monotonic()
                except (httpx.HTTPError, ValueError, KeyError) as exc:
                    if not self._keys:
                        raise InvalidTokenError(f"jwks unavailable: {exc}") from exc
            if kid not in self._keys:
                raise InvalidTokenError(f"unknown kid {kid!r}")
            return self._keys[kid]


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
