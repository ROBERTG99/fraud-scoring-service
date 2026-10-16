"""Route configuration (config/llm_routes.yaml): provider, model, effort, limits and prices."""

from __future__ import annotations

import os
from dataclasses import dataclass
from functools import lru_cache
from pathlib import Path
from typing import Any

import yaml

from fraud_summarizer.paths import repo_root


@dataclass(frozen=True)
class Route:
    name: str
    provider: str
    fallback_provider: str | None
    models: dict[str, str]
    effort: str
    max_tokens: int
    timeout_s: float
    attempts: int
    prompt: str

    def model_for(self, provider: str) -> str:
        return self.models[provider]


@dataclass(frozen=True)
class RouteConfig:
    routes: dict[str, Route]
    pricing: dict[str, tuple[float, float]]

    def cost_usd(self, model: str, input_tokens: int, output_tokens: int) -> float:
        inp, out = self.pricing.get(model, (0.0, 0.0))
        return round(input_tokens * inp / 1e6 + output_tokens * out / 1e6, 6)


def _resolve(raw: dict[str, Any], env: dict[str, str]) -> dict[str, Route]:
    out = {}
    for name, r in raw["routes"].items():
        provider = env.get(r.get("provider_env", ""), "") or r["default_provider"]
        fallback = env.get(r.get("fallback_provider_env", ""), "") or None
        models = {p: (env.get(m.get("env", ""), "") or m["default"]) for p, m in r["models"].items()}
        if provider not in models:
            raise ValueError(f"route {name}: unknown provider {provider!r}")
        if fallback is not None and fallback not in models:
            raise ValueError(f"route {name}: unknown fallback provider {fallback!r}")
        out[name] = Route(
            name,
            provider,
            fallback if fallback != provider else None,
            models,
            r["effort"],
            int(r["max_tokens"]),
            float(r["timeout_s"]),
            int(r["attempts"]),
            r["prompt"],
        )
    return out


def load(path: Path | None = None, env: dict[str, str] | None = None) -> RouteConfig:
    raw = _load_raw(str(path or repo_root() / "config" / "llm_routes.yaml"))
    return RouteConfig(
        routes=_resolve(raw, dict(os.environ) if env is None else env),
        pricing={k: (float(v[0]), float(v[1])) for k, v in raw["pricing"].items()},
    )


@lru_cache(maxsize=4)
def _load_raw(path: str) -> dict[str, Any]:
    data = yaml.safe_load(Path(path).read_text(encoding="utf-8"))
    assert data["schema"] == "fraud.llm_routes.v1"
    return data  # type: ignore[no-any-return]
