"""LLM gateway: provider selection, timeouts, retries with backoff and jitter, fallback provider, token
and cost accounting, tracing and a last-line PII redaction. Every call returns or raises with the full
attempt history so the pipeline can write step records.

Retries follow Appendix B exactly: only 408, 409, 429, 5xx, timeouts and connection errors. Invalid JSON or a
schema violation is not retried on the same provider; the call moves to the fallback provider, if any, and
otherwise the pipeline writes a `fallback` summary.
"""

from __future__ import annotations

import asyncio
import json
import random
import time
from collections.abc import Awaitable, Callable, Mapping
from dataclasses import dataclass, field
from typing import Any

from opentelemetry import trace

from fraud_summarizer.llm.routes import RouteConfig
from fraud_summarizer.llm.types import Provider, ProviderError, ProviderRequest, ProviderResponse
from fraud_summarizer.redact import redact_text
from fraud_summarizer.schemas import provider_schema

tracer = trace.get_tracer("fraud_summarizer.llm")


@dataclass
class Attempt:
    provider: str
    model: str
    status: str  # ok | error
    error: str | None
    error_kind: str | None
    latency_ms: float
    input_tokens: int = 0
    output_tokens: int = 0
    cost_usd: float = 0.0


@dataclass
class LLMResult:
    data: dict[str, Any]
    text: str
    provider: str
    model: str
    prompt_version: str
    input_tokens: int
    output_tokens: int
    cost_usd: float
    latency_ms: float
    attempts: list[Attempt] = field(default_factory=list)
    cassette: str | None = None


class GatewayExhaustedError(Exception):
    def __init__(self, message: str, attempts: list[Attempt]) -> None:
        super().__init__(message)
        self.attempts = attempts


Sleep = Callable[[float], Awaitable[None]]


class LLMGateway:
    def __init__(
        self,
        config: RouteConfig,
        providers: Mapping[str, Provider],
        *,
        sleep: Sleep = asyncio.sleep,
        rng: random.Random | None = None,
        base_delay_s: float = 0.25,
        max_delay_s: float = 2.0,
    ) -> None:
        self.config = config
        self.providers = providers
        self.sleep = sleep
        self.rng = rng or random.Random(7)  # noqa: S311 - jitter, not security
        self.base_delay_s = base_delay_s
        self.max_delay_s = max_delay_s

    def backoff(self, attempt: int) -> float:
        delay = min(self.max_delay_s, self.base_delay_s * (2 ** (attempt - 1)))
        return float(delay * (0.5 + self.rng.random() / 2))  # equal jitter

    async def complete(
        self,
        route_name: str,
        *,
        system: str,
        user: str,
        schema_name: str,
        schema: dict[str, Any],
        prompt_version: str,
        validate: Callable[[dict[str, Any]], None] | None = None,
    ) -> LLMResult:
        route = self.config.routes[route_name]
        chain = [route.provider] + ([route.fallback_provider] if route.fallback_provider else [])
        attempts: list[Attempt] = []
        safe_system, safe_user = redact_text(system), redact_text(user)
        start = time.perf_counter()
        with tracer.start_as_current_span(f"llm.{route_name}") as span:
            span.set_attribute("gen_ai.operation.name", "chat")
            span.set_attribute("app.prompt_version", prompt_version)
            for provider_name in chain:
                provider = self.providers[provider_name]
                model = route.model_for(provider_name)
                req = ProviderRequest(
                    route=route_name,
                    model=model,
                    system=safe_system,
                    user=safe_user,
                    schema_name=schema_name,
                    schema=provider_schema(schema),
                    max_tokens=route.max_tokens,
                    effort=route.effort,
                    timeout_s=route.timeout_s,
                    prompt_version=prompt_version,
                )
                for n in range(1, route.attempts + 1):
                    t0 = time.perf_counter()
                    try:
                        resp = await self._call(provider, provider_name, req, route.timeout_s)
                        data = self._parse(resp, validate)
                    except ProviderError as exc:
                        err = exc
                    else:
                        cost = self.config.cost_usd(
                            resp.model,
                            resp.input_tokens,
                            resp.output_tokens,
                            cache_write_tokens=resp.cache_write_tokens,
                            cache_read_tokens=resp.cache_read_tokens,
                        )
                        attempts.append(
                            Attempt(
                                provider_name,
                                resp.model,
                                "ok",
                                None,
                                None,
                                (time.perf_counter() - t0) * 1000,
                                resp.input_tokens,
                                resp.output_tokens,
                                cost,
                            )
                        )
                        span.set_attribute("gen_ai.system", provider_name)
                        span.set_attribute("gen_ai.request.model", model)
                        span.set_attribute("gen_ai.response.model", resp.model)
                        span.set_attribute("gen_ai.usage.input_tokens", resp.input_tokens)
                        span.set_attribute("gen_ai.usage.output_tokens", resp.output_tokens)
                        span.set_attribute("app.cost_usd", cost)
                        span.set_attribute("app.attempts", len(attempts))
                        return LLMResult(
                            data=data,
                            text=resp.text,
                            provider=provider_name,
                            model=resp.model,
                            prompt_version=prompt_version,
                            input_tokens=sum(a.input_tokens for a in attempts),
                            output_tokens=sum(a.output_tokens for a in attempts),
                            cost_usd=round(sum(a.cost_usd for a in attempts), 6),
                            latency_ms=(time.perf_counter() - start) * 1000,
                            attempts=attempts,
                            cassette=resp.extra.get("cassette"),
                        )
                    attempts.append(
                        Attempt(
                            provider_name,
                            model,
                            "error",
                            str(err),
                            err.kind,
                            (time.perf_counter() - t0) * 1000,
                        )
                    )
                    if not err.retryable or n == route.attempts:
                        break
                    await self.sleep(self.backoff(n))
            span.set_attribute("app.attempts", len(attempts))
            span.set_attribute("app.status", "exhausted")
        raise GatewayExhaustedError(f"route {route_name}: {len(attempts)} failed attempts", attempts)

    @staticmethod
    async def _call(
        provider: Provider, provider_name: str, req: ProviderRequest, timeout_s: float
    ) -> ProviderResponse:
        """Calls the provider; every failure of the call itself becomes a ProviderError."""
        try:
            return await asyncio.wait_for(provider.complete(req), timeout=timeout_s)
        except TimeoutError:
            raise ProviderError(
                f"{provider_name} timed out after {timeout_s}s", retryable=True, kind="timeout"
            ) from None
        except ProviderError:
            raise
        except Exception as exc:  # a provider bug (e.g. SDK TypeError) must not stall the caller
            raise ProviderError(f"{type(exc).__name__}: {exc}"[:300], kind="provider_error") from exc

    @staticmethod
    def _parse(resp: ProviderResponse, validate: Callable[[dict[str, Any]], None] | None) -> dict[str, Any]:
        try:
            data = json.loads(resp.text)
        except json.JSONDecodeError as exc:
            raise ProviderError(f"invalid JSON from model: {exc}", kind="invalid_json") from exc
        if not isinstance(data, dict):
            raise ProviderError("model output is not a JSON object", kind="invalid_json")
        if validate is not None:
            try:
                validate(data)
            except ValueError as exc:  # pydantic.ValidationError is a ValueError
                raise ProviderError(f"schema validation failed: {exc}", kind="schema") from exc
        return data
