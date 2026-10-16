"""Build the gateway with the providers a route configuration needs."""

from __future__ import annotations

from pathlib import Path

from fraud_summarizer.llm.cassettes import CassetteStore
from fraud_summarizer.llm.fake import FakeLLM
from fraud_summarizer.llm.gateway import LLMGateway
from fraud_summarizer.llm.routes import RouteConfig
from fraud_summarizer.llm.types import Provider
from fraud_summarizer.paths import repo_root


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

        providers["anthropic"] = AnthropicProvider()
    if "openai" in needed:
        from fraud_summarizer.llm.openai_adapter import OpenAIProvider

        providers["openai"] = OpenAIProvider()
    return LLMGateway(config, providers)
