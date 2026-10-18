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
class Usage:
    """What one provider response was billed for: uncached input, output, cache writes and cache reads."""

    model: str
    input_tokens: int
    output_tokens: int
    cache_write_tokens: int = 0
    cache_read_tokens: int = 0


@dataclass(frozen=True)
class ProviderResponse:
    """input_tokens is the uncached input; cache writes and reads are counted apart, each at its own price."""

    text: str
    model: str
    input_tokens: int
    output_tokens: int
    stop_reason: str
    cache_read_tokens: int = 0
    cache_write_tokens: int = 0
    extra: dict[str, Any] = field(default_factory=dict)


class ProviderError(Exception):
    """A provider call failed. `retryable` follows Appendix B: 408, 409, 429, 5xx and connection errors."""

    def __init__(
        self,
        message: str,
        *,
        status: int | None = None,
        retryable: bool = False,
        kind: str = "error",
        usage: Usage | None = None,
    ) -> None:
        super().__init__(message)
        self.status = status
        self.retryable = retryable
        self.kind = kind
        # The usage of a response that was billed but is unusable (refusal, max_tokens, no text): the gateway
        # charges it against the budget (D-062, D-064).
        self.usage = usage

    @classmethod
    def from_status(cls, status: int, message: str) -> ProviderError:
        return cls(message, status=status, retryable=status in RETRYABLE_STATUS or status >= 500, kind="http")


class Provider(Protocol):
    name: str

    async def complete(self, req: ProviderRequest) -> ProviderResponse: ...
