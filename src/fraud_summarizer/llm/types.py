"""Provider-neutral request/response types and errors."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Protocol

RETRYABLE_STATUS = frozenset({408, 409, 429, 500, 502, 503, 504, 529})


@dataclass(frozen=True)
class ProviderRequest:
    route: str
    model: str
    system: str
    user: str
    schema_name: str
    schema: dict[str, Any]  # provider-safe JSON schema (see schemas.provider_schema)
    max_tokens: int
    effort: str
    timeout_s: float
    prompt_version: str


@dataclass(frozen=True)
class ProviderResponse:
    text: str
    model: str
    input_tokens: int
    output_tokens: int
    stop_reason: str
    cache_read_tokens: int = 0
    extra: dict[str, Any] = field(default_factory=dict)


class ProviderError(Exception):
    """A provider call failed. `retryable` follows Appendix B: 408, 409, 429, 5xx and connection errors."""

    def __init__(
        self, message: str, *, status: int | None = None, retryable: bool = False, kind: str = "error"
    ) -> None:
        super().__init__(message)
        self.status = status
        self.retryable = retryable
        self.kind = kind

    @classmethod
    def from_status(cls, status: int, message: str) -> ProviderError:
        return cls(message, status=status, retryable=status in RETRYABLE_STATUS or status >= 500, kind="http")


class Provider(Protocol):
    name: str

    async def complete(self, req: ProviderRequest) -> ProviderResponse: ...
