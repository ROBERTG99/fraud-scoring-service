"""Dev-issuer JWT verification (EdDSA, JWKS), mirroring internal/auth/jwt.go."""

from __future__ import annotations

import asyncio
import getpass
import json
import math
import os
import time
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path
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


def check_database_dsn(dsn: str) -> None:
    """Refuse, outside FRAUD_ENV=dev, credentials without a password or client certificate, or with the
    migrations' development password (the role's own name), as internal/auth.CheckDatabaseDSN. Resolves what
    libpq would use: URL or keyword/value, decoded, query parameters, the service file, PGUSER, PGPASSWORD,
    the matching passfile entry, and a client certificate only when it exists and TLS is on (round 3 and 4)."""
    if os.environ.get("FRAUD_ENV") == "dev":
        return
    from psycopg import ProgrammingError
    from psycopg.conninfo import conninfo_to_dict

    try:
        params = {k: str(v) for k, v in conninfo_to_dict(dsn).items() if v is not None}
    except ProgrammingError as exc:
        raise ValueError(f"database DSN: {exc}") from exc
    service = params.get("service") or os.environ.get("PGSERVICE")
    if service:
        params = _service_params(service) | params  # the DSN's own parameters win
    user = params.get("user") or os.environ.get("PGUSER") or getpass.getuser()
    password = params.get("password") or os.environ.get("PGPASSWORD")
    # libpq tries every host in turn, each with its own passfile entry when no password is set (round 5).
    passwords = [password] if password else _passfile_passwords(params, user)
    for pw in passwords:
        if not pw:
            cert = params.get("sslcert") or os.environ.get("PGSSLCERT")
            sslmode = params.get("sslmode") or os.environ.get("PGSSLMODE") or "prefer"
            if cert and sslmode != "disable" and Path(cert).is_file():
                continue  # certificate authentication: nothing here to compare
            raise ValueError(f"database DSN for role {user}: no password or client certificate")
        if pw == user:
            raise ValueError(
                f"database DSN for role {user}: development credentials are allowed only with FRAUD_ENV=dev"
            )


def _service_params(service: str) -> dict[str, str]:
    """The named section of the libpq service file (PGSERVICEFILE, ~/.pg_service.conf, then
    PGSYSCONFDIR/pg_service.conf), parsed as libpq does: each line stripped, "#" comments, "[name]" sections,
    key=value lines and nothing else (round 5: ConfigParser read an indented line as a continuation)."""
    candidates = [os.environ.get("PGSERVICEFILE") or str(Path.home() / ".pg_service.conf")]
    if os.environ.get("PGSYSCONFDIR"):
        candidates.append(str(Path(os.environ["PGSYSCONFDIR"]) / "pg_service.conf"))
    for path in candidates:
        try:
            lines = Path(path).read_text(errors="replace").splitlines()
        except OSError:
            continue
        found: dict[str, str] | None = None
        for n, raw in enumerate(lines, 1):
            line = raw.strip()
            if not line or line.startswith("#"):
                continue
            if line.startswith("["):
                if found is not None:
                    break  # the next section ends ours
                found = {} if line == f"[{service}]" else None
                continue
            if found is None:
                continue  # libpq reads only the lines of the section it looks for
            key, eq, value = line.partition("=")
            if not eq:
                raise ValueError(f"database DSN: service file {path}, line {n}: not key=value")
            found[key.strip()] = value.strip()
        if found is not None:
            return found
    raise ValueError(f"database DSN: service {service!r} not found")


def _passfile_fields(line: str) -> list[str]:
    fields: list[str] = []
    cur, escaped = "", False
    for ch in line:
        if escaped:
            cur, escaped = cur + ch, False
        elif ch == "\\":
            escaped = True
        elif ch == ":" and len(fields) < 4:
            fields.append(cur)
            cur = ""
        else:
            cur += ch
    return [*fields, cur]


# libpq's default socket directories: a socket there is "localhost" in the passfile, any other is its path.
DEFAULT_SOCKET_DIRS = frozenset({"/var/run/postgresql", "/run/postgresql", "/tmp"})  # noqa: S108 -- names only


def _passfile_passwords(params: dict[str, str], user: str) -> list[str]:
    """For each host libpq would try, the password of the first matching passfile entry ("" for none)."""
    path = Path(params.get("passfile") or os.environ.get("PGPASSFILE") or Path.home() / ".pgpass")
    try:
        lines = path.read_text(errors="replace").splitlines()
    except OSError:
        lines = []
    entries = [
        fields
        for line in lines
        if line.strip() and not line.startswith("#")
        for fields in [_passfile_fields(line)]
        if len(fields) == 5
    ]
    hosts = (params.get("host") or os.environ.get("PGHOST") or "localhost").split(",")
    ports = (params.get("port") or os.environ.get("PGPORT") or "5432").split(",")
    dbname = params.get("dbname") or os.environ.get("PGDATABASE") or user
    out = []
    for i, raw_host in enumerate(hosts):
        host = raw_host.strip() or "localhost"
        if host.startswith("/") and host.rstrip("/") in DEFAULT_SOCKET_DIRS:
            host = "localhost"
        port = (ports[i] if len(ports) == len(hosts) else ports[0]).strip() or "5432"
        want = (host, port, dbname, user)
        out.append(
            next((f[4] for f in entries if all(x in ("*", w) for x, w in zip(f[:4], want, strict=True))), "")
        )
    return out


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
JWKS_MAX_BYTES = 64 << 10  # as internal/auth


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
        self._inflight: asyncio.Task[None] | None = None

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
            # Detached from the caller, as in Go: cancelling one request must not fail every waiter's fetch.
            self._inflight = asyncio.get_running_loop().create_task(self._refresh())
            await asyncio.shield(self._inflight)
            # Loop once more: the new keys, or (tried is now recent) the stale key or an error.

    async def _refresh(self) -> None:
        try:
            self._keys, self._fetched = await self._fetch(), self.clock()
            self._last_error = None
        except (httpx.HTTPError, ValueError, KeyError) as exc:
            self._last_error = exc
        finally:
            self._inflight = None

    async def _fetch(self) -> dict[str, Any]:
        async with (
            httpx.AsyncClient(timeout=self.timeout_s, transport=self.transport) as client,
            client.stream("GET", self.url) as resp,
        ):
            resp.raise_for_status()
            body = b""
            async for chunk in resp.aiter_bytes():
                body += chunk
                if len(body) > JWKS_MAX_BYTES:
                    raise ValueError(f"jwks larger than {JWKS_MAX_BYTES} bytes")
            return StaticKeys(json.loads(body)).keys


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
        if not str(claims["sub"]).strip():
            raise InvalidTokenError("empty sub")
        role = str(claims.get("role", ""))
        if role not in CONSOLE_ROLES:
            raise InvalidTokenError(f"unknown role {role!r}")
        return Principal(subject=str(claims["sub"]), role=role)
