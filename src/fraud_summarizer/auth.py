"""Dev-issuer JWT verification (EdDSA, JWKS), mirroring internal/auth/jwt.go."""

from __future__ import annotations

import asyncio
import getpass
import json
import math
import os
import stat
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
    user = _option(params, "user", "PGUSER") or _effective_user()
    # A password option that is present, even empty, hides PGPASSWORD; an empty one falls to the passfile (round 6).
    password = params["password"] if "password" in params else os.environ.get("PGPASSWORD", "")
    # libpq tries every host in turn, each with its own passfile entry when no password is set (round 5). A
    # socket path may be looked up as "localhost" or as itself, so both count (round 6).
    candidates = [[password]] if password else _passfile_passwords(params, user)
    for cands in candidates:
        if user in cands:
            raise ValueError(
                f"database DSN for role {user}: development credentials are allowed only with FRAUD_ENV=dev"
            )
        pw = next((c for c in cands if c), "")
        if not pw:
            # As the other options: an empty sslcert is libpq's default file, never PGSSLCERT (round 9).
            cert = _option(params, "sslcert", "PGSSLCERT") or str(
                Path.home() / ".postgresql" / "postgresql.crt"
            )
            sslmode = _option(params, "sslmode", "PGSSLMODE") or "prefer"
            if cert and sslmode != "disable" and Path(cert).is_file():
                continue  # certificate authentication: nothing here to compare
            raise ValueError(f"database DSN for role {user}: no password or client certificate")


def _option(params: dict[str, str], key: str, env: str) -> str:
    """A connection option as libpq resolves it: the DSN's (or service's) value when the option is present,
    even empty, and the environment only when it is absent (round 8: an empty option never reads the
    environment). "" means libpq's built-in default."""
    return params[key] if key in params else os.environ.get(env, "")


LIBPQ_SPACE = " \t\r\v\f"  # what C's isspace() strips, not every Unicode line break as str.strip() does


def _effective_user() -> str:
    """The user libpq defaults to: the effective uid's name, not $USER or $LOGNAME (round 7)."""
    try:
        import pwd

        return pwd.getpwuid(os.geteuid()).pw_name
    except (ImportError, KeyError, AttributeError):
        return getpass.getuser()


def _service_params(service: str) -> dict[str, str]:
    """The named section of the libpq service file (PGSERVICEFILE, ~/.pg_service.conf, then
    PGSYSCONFDIR/pg_service.conf), parsed as libpq does: lines end at "\\n" only, each is stripped, "#"
    comments, "[name]" sections, key=value lines and nothing else, and the first value of a key wins (rounds 5
    and 6: ConfigParser read an indented line as a continuation)."""
    candidates = [os.environ.get("PGSERVICEFILE") or str(Path.home() / ".pg_service.conf")]
    if os.environ.get("PGSYSCONFDIR"):
        candidates.append(str(Path(os.environ["PGSYSCONFDIR"]) / "pg_service.conf"))
    for path in candidates:
        try:
            lines = Path(path).read_bytes().decode(errors="replace").split("\n")  # no universal newlines
        except OSError:
            continue
        found: dict[str, str] | None = None
        for n, raw in enumerate(lines, 1):
            line = raw.strip(LIBPQ_SPACE)
            if not line or line.startswith("#"):
                continue
            if line.startswith("["):
                if found is not None:
                    break  # the next section ends ours
                found = {} if line.startswith(f"[{service}]") else None  # libpq compares up to "]"
                continue
            if found is None:
                continue  # libpq reads only the lines of the section it looks for
            key, eq, value = line.partition("=")
            if not eq:
                raise ValueError(f"database DSN: service file {path}, line {n}: not key=value")
            found.setdefault(key.strip(LIBPQ_SPACE), value.strip(LIBPQ_SPACE))
        if found is not None:
            return found
    raise ValueError(f"database DSN: service {service!r} not found")


def _passfile_fields(line: str) -> list[tuple[str, bool]]:
    """A passfile line as libpq reads it: (value, is a bare "*" wildcard) per field. Fields end at an unescaped
    ":" and the password at the next one (round 7: "pw:old" is "pw" to libpq); an escaped "\\*" is a literal."""
    fields: list[tuple[str, bool]] = []
    cur, raw, escaped = "", "", False
    for ch in line:
        if escaped:
            cur, raw, escaped = cur + ch, raw + "\\" + ch, False
        elif ch == "\\":
            escaped = True
        elif ch == ":":
            fields.append((cur, raw == "*"))
            if len(fields) == 5:
                return fields
            cur, raw = "", ""
        else:
            cur, raw = cur + ch, raw + ch
    return [*fields, (cur, raw == "*")]


def _passfile_readable(path: Path) -> bool:
    """libpq ignores a passfile that is not a regular file or that its group or others can read (round 7)."""
    try:
        st = path.stat()
    except OSError:
        return False
    return stat.S_ISREG(st.st_mode) and not st.st_mode & 0o077


def _passfile_passwords(params: dict[str, str], user: str) -> list[list[str]]:
    """For each host libpq would try, the passwords of the first matching passfile entry ("" for none): one
    for a host name or address, two for a socket path, which libpq looks up as "localhost" when it is its
    compiled default directory and as the path otherwise (round 6: both count, failing closed)."""
    path = Path(_option(params, "passfile", "PGPASSFILE") or Path.home() / ".pgpass")
    try:
        lines = path.read_bytes().decode(errors="replace").split("\n") if _passfile_readable(path) else []
    except OSError:
        lines = []
    entries = [
        fields
        for raw in lines
        for line in [raw.rstrip("\r")]
        if line.strip(LIBPQ_SPACE) and not line.startswith("#")
        for fields in [_passfile_fields(line)]
        if len(fields) == 5
    ]
    # libpq matches the passfile on each host, or on its hostaddr when that host entry is empty (rounds 6, 8).
    host_opt, addr_opt = _option(params, "host", "PGHOST"), _option(params, "hostaddr", "PGHOSTADDR")
    hosts = host_opt.split(",") if host_opt else addr_opt.split(",") if addr_opt else [""]
    addrs = addr_opt.split(",") if addr_opt else []
    ports = (_option(params, "port", "PGPORT") or "5432").split(",")
    dbname = _option(params, "dbname", "PGDATABASE") or user

    def lookup(host: str, port: str) -> str:
        want = (host, port, dbname, user)
        return next(
            (
                f[4][0]
                for f in entries
                if all(star or x == w for (x, star), w in zip(f[:4], want, strict=True))
            ),
            "",
        )

    out = []
    # Entries are used as written, spaces included, as libpq uses them (round 9); only an empty one defaults.
    for i, raw_host in enumerate(hosts):
        addr = addrs[i] if i < len(addrs) else ""
        host = raw_host or addr or "localhost"
        port = (ports[i] if len(ports) == len(hosts) else ports[0]) or "5432"
        names_tried = ["localhost", host] if host.startswith("/") else [host]
        out.append([lookup(h, port) for h in names_tried])
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
