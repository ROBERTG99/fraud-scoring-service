"""Build the gateway with the providers a route configuration needs."""

from __future__ import annotations

from pathlib import Path

from fraud_summarizer.llm.budget import budget_from_env
from fraud_summarizer.llm.cassettes import CassetteStore
from fraud_summarizer.llm.fake import FakeLLM
from fraud_summarizer.llm.gateway import LLMGateway
from fraud_summarizer.llm.routes import RouteConfig
from fraud_summarizer.llm.types import Provider, ProviderError, ProviderRequest, ProviderResponse
from fraud_summarizer.paths import repo_root


class UnavailableProvider:
    """Stands in for a provider whose client could not be built (for example a missing API key), so the
    gateway moves to the fallback provider or the templated summary instead of the service failing to start."""

    def __init__(self, name: str, reason: str) -> None:
        self.name = name
        self.reason = reason

    async def complete(self, req: ProviderRequest) -> ProviderResponse:
        raise ProviderError(f"{self.name} unavailable: {self.reason}", retryable=False, kind="unavailable")


def build_gateway(config: RouteConfig, cassette_dir: Path | None = None) -> LLMGateway:
    needed = {r.provider for r in config.routes.values()} | {
        r.fallback_provider for r in config.routes.values() if r.fallback_provider
    }
    providers: dict[str, Provider] = {}
    route = config.routes["case_summary"]
    real_model = route.models.get("anthropic")
    providers["fake"] = FakeLLM(
        cassettes=CassetteStore(cassette_dir or repo_root() / "tests" / "fixtures" / "llm"),
        cassette_model=real_model,
    )
    if "anthropic" in needed:
        from fraud_summarizer.llm.anthropic_adapter import AnthropicProvider

        providers["anthropic"] = _build("anthropic", AnthropicProvider)
    if "openai" in needed:
        from fraud_summarizer.llm.openai_adapter import OpenAIProvider

        providers["openai"] = _build("openai", OpenAIProvider)
    return LLMGateway(config, providers, budget=budget_from_env())


def _build(name: str, factory: type[Provider]) -> Provider:
    try:
        return factory()
    except Exception as exc:  # SDK constructors raise their own error types for missing credentials
        return UnavailableProvider(name, f"{type(exc).__name__}: {exc}"[:200])
