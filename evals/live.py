"""Live provider runs for `make eval-live` and `make record-fixtures`, bounded by a USD budget.

Every provider response is charged at the route's list price (`config/llm_routes.yaml` pricing); once the
spend reaches the budget no further case starts. Optionally each response is stored as a cassette, which
FakeLLM then replays offline (`tests/fixtures/llm`).
"""

from __future__ import annotations

import asyncio
import os
from dataclasses import dataclass
from typing import Any

from evalgate.contracts import TargetResult
from evals.graders.summaries import summarize_with
from fraud_summarizer.llm import LLMGateway
from fraud_summarizer.llm.cassettes import CassetteStore
from fraud_summarizer.llm.fake import FakeLLM
from fraud_summarizer.llm.routes import RouteConfig, load
from fraud_summarizer.llm.types import Provider, ProviderRequest, ProviderResponse

KEY_VARS = {"anthropic": ("ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"), "openai": ("OPENAI_API_KEY",)}


class BudgetExceededError(RuntimeError):
    pass


@dataclass
class Budget:
    limit_usd: float
    spent_usd: float = 0.0

    @property
    def exhausted(self) -> bool:
        return self.spent_usd >= self.limit_usd


def missing_key(provider: str) -> str | None:
    """The environment variable to set when `provider` has no credentials, else None."""
    names = KEY_VARS.get(provider, ())
    return None if any(os.environ.get(n) for n in names) else (names[0] if names else None)


def provider_for(name: str) -> Provider:
    """The real provider adapter (imports the SDK only here)."""
    if name == "anthropic":
        from fraud_summarizer.llm.anthropic_adapter import AnthropicProvider

        return AnthropicProvider()
    if name == "openai":
        from fraud_summarizer.llm.openai_adapter import OpenAIProvider

        return OpenAIProvider()
    raise ValueError(f"no live provider {name!r}")


@dataclass
class _Charging:
    inner: Provider
    budget: Budget
    config: RouteConfig
    store: CassetteStore | None = None

    @property
    def name(self) -> str:
        return str(getattr(self.inner, "name", "provider"))

    async def complete(self, req: ProviderRequest) -> ProviderResponse:
        if self.budget.exhausted:
            raise BudgetExceededError(f"live budget USD {self.budget.limit_usd} spent")
        resp = await self.inner.complete(req)
        self.budget.spent_usd += self.config.cost_usd(
            resp.model,
            resp.input_tokens,
            resp.output_tokens,
            cache_write_tokens=resp.cache_write_tokens,
            cache_read_tokens=resp.cache_read_tokens,
        )
        if self.store is not None:
            self.store.put(req, resp, recorded=True)
        return resp


def live_gateway(env: dict[str, str], budget: Budget, store: CassetteStore | None = None) -> LLMGateway:
    config = load(env=env)
    providers: dict[str, Provider] = {"fake": FakeLLM()}
    needed = {r.provider for r in config.routes.values()} | {
        r.fallback_provider for r in config.routes.values() if r.fallback_provider
    }
    for name in sorted(needed - {"fake"}):
        providers[name] = _Charging(provider_for(name), budget, config, store)
    return LLMGateway(config, providers)


@dataclass
class CaseResult:
    id: str
    result: TargetResult | None
    error: str | None


def summarize_cases(rows: list[dict[str, Any]], gateway: LLMGateway) -> list[CaseResult]:
    """Summarize cases one at a time (the budget is checked before each case)."""
    budget = next((p.budget for p in gateway.providers.values() if isinstance(p, _Charging)), None)

    async def run() -> list[CaseResult]:
        out = []
        for row in rows:
            if budget is not None and budget.exhausted:
                out.append(CaseResult(row["id"], None, f"not run: live budget USD {budget.limit_usd} spent"))
                continue
            try:
                out.append(CaseResult(row["id"], await summarize_with(gateway, row["input"]), None))
            except Exception as exc:  # reported per case, never dropped
                out.append(CaseResult(row["id"], None, f"{type(exc).__name__}: {exc}"[:300]))
        return out

    return asyncio.run(run())
