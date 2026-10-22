"""`summaries` suite: target and graders.

The target runs the real case-summary pipeline (redaction, gateway, schema validation, faithfulness) on one
case with the providers of `config/llm_routes.yaml`: FakeLLM offline (`FRAUD_SUMMARY_PROVIDER=fake`, the
`make eval-summary` default), the configured model under `make eval-live`. Every request that reaches a
provider is recorded, so the PII grader checks exactly what left the process.
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field
from functools import lru_cache
from typing import Any

import evalgate
from evalgate.contracts import Case, TargetResult
from fraud_summarizer.llm import LLMGateway
from fraud_summarizer.llm.factory import build_gateway
from fraud_summarizer.llm.routes import load
from fraud_summarizer.llm.types import Provider, ProviderRequest, ProviderResponse
from fraud_summarizer.pipeline import CaseContext, PipelineResult, StepRecord, SummaryPipeline


@lru_cache(maxsize=1)
def _base() -> LLMGateway:
    env = dict(os.environ)
    env.setdefault("FRAUD_SUMMARY_PROVIDER", "fake")
    return build_gateway(load(env=env))


@dataclass
class _Recorder:
    inner: Provider
    sent: list[str]

    @property
    def name(self) -> str:
        return str(getattr(self.inner, "name", "provider"))

    async def complete(self, req: ProviderRequest) -> ProviderResponse:
        self.sent.append(f"{req.system}\n{req.user}")
        return await self.inner.complete(req)


@dataclass
class _OneCase:
    ctx: CaseContext
    saved: list[PipelineResult] = field(default_factory=list)

    async def load_case(self, decision_id: str) -> CaseContext | None:
        return self.ctx

    async def save_summary(self, result: PipelineResult, *, replace: bool = False) -> None:
        self.saved.append(result)

    async def save_steps(self, steps: list[StepRecord]) -> None:
        return None


async def target(row: dict[str, Any]) -> TargetResult:
    return await summarize_with(_base(), row)


async def summarize_with(base: LLMGateway, row: dict[str, Any]) -> TargetResult:
    """Run the summary pipeline on one case through `base`'s providers, recording every provider request."""
    sent: list[str] = []
    providers = {name: _Recorder(p, sent) for name, p in base.providers.items()}
    gateway = LLMGateway(base.config, providers)
    ctx = CaseContext(
        case_id=row["case_id"], decision=row["decision"], features=row["features"], events=row["events"]
    )
    res = await SummaryPipeline(gateway, _OneCase(ctx)).run(row["decision"]["decision_id"], run_id="eval")
    if res is None:
        raise RuntimeError("case not loaded")
    s = res.summary
    output = {
        "status": res.status,
        "pattern": s.pattern,
        "faithfulness": s.faithfulness,
        "confidence": s.confidence,
        "model": s.model,
        "prompt_version": s.prompt_version,
        "evidence": [{"fact_key": e.fact_key, "statement": e.statement} for e in s.evidence],
        "recommended_checks": list(s.recommended_checks),
        "facts": dict(s.facts),
        "provider_prompt": "\n".join(sent),
        "prompt_hash": res.prompt_hash,
    }
    return TargetResult(output=output, steps=[{"node": "summarize", "model": s.model}], cost_usd=res.cost_usd)


def _out(result: TargetResult) -> dict[str, Any]:
    return result.output if isinstance(result.output, dict) else {}


@evalgate.metric("schema_valid")
def schema_valid(case: Case, result: TargetResult) -> float:
    """The model's output passed schema validation (a templated fallback means it did not)."""
    return float(_out(result).get("status") in ("ok", "unfaithful"))


@evalgate.metric("faithfulness")
def faithfulness(case: Case, result: TargetResult) -> float:
    """Share of evidence items whose fact key and numbers match the case facts (pipeline check)."""
    return float(_out(result).get("faithfulness", 0.0))


@evalgate.metric("pii_to_provider")
def pii_to_provider(case: Case, result: TargetResult) -> float:
    """1 when any synthetic PII planted in the case reached a provider request (gate: 0)."""
    prompt = str(_out(result).get("provider_prompt", ""))
    return float(any(p in prompt for p in case.meta.get("pii", [])))
