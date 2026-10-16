"""Case-summary pipeline (spec §4.6): a fixed sequence of steps, not an agent.

load_facts → redact → summarize (llm) → validate_schema → check_faithfulness → persist

Each step writes one step record. The pipeline only ever writes `case_summaries` and `step_records`;
it has no write path to `decisions` and the model has no tools.
"""

from __future__ import annotations

import hashlib
import time
import uuid
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field
from datetime import UTC, datetime
from typing import Any, Literal, Protocol

from pydantic import ValidationError

from fraud_summarizer import facts as factfmt
from fraud_summarizer.faithfulness import check as check_faithfulness
from fraud_summarizer.llm import GatewayExhaustedError, LLMGateway, LLMResult
from fraud_summarizer.paths import repo_root
from fraud_summarizer.redact import redact_text
from fraud_summarizer.schemas import (
    CaseSummaryOutput,
    StoredSummary,
    VerifiedEvidence,
    case_summary_json_schema,
)

PROMPT_VERSION = "case_summary.v1"
FALLBACK_PROMPT_VERSION = "fallback.v1"
MAX_EVENTS = 50
MAX_TEXT = 120

Status = Literal["ok", "fallback", "unfaithful"]


@dataclass
class CaseContext:
    case_id: str
    decision: dict[
        str, Any
    ]  # decision_id, transaction_id, account_id, action, score, reason_codes, model_version, rules_version, degraded, decided_at
    features: dict[str, float]
    events: list[dict[str, Any]]  # {"kind", "event_ts", "payload"} newest first


@dataclass
class StepRecord:
    run_id: str
    task_id: str | None
    step: int
    node: str
    kind: str
    model: str | None
    prompt_version: str | None
    input_tokens: int
    output_tokens: int
    cost_usd: float
    latency_ms: float
    status: str
    error: str | None
    started_at: datetime


@dataclass
class PipelineResult:
    case_id: str
    status: Status
    summary: StoredSummary
    steps: list[StepRecord]
    cost_usd: float
    facts: dict[str, str] = field(default_factory=dict)
    prompt_hash: str = ""


class CaseRepository(Protocol):
    async def load_case(self, decision_id: str) -> CaseContext | None: ...

    async def save_summary(self, result: PipelineResult) -> None: ...

    async def save_steps(self, steps: list[StepRecord]) -> None: ...


def fmt_number(x: float) -> str:
    if float(x).is_integer():
        return str(int(x))
    return f"{x:.4f}".rstrip("0").rstrip(".")


def _text(value: Any) -> str:
    s = str(value)
    return s if len(s) <= MAX_TEXT else s[:MAX_TEXT]


def build_facts(ctx: CaseContext) -> dict[str, str]:
    d = ctx.decision
    out: dict[str, str] = {
        "decision.action": str(d["action"]),
        "decision.score": fmt_number(float(d["score"])) if d.get("score") is not None else "-1",
        "decision.reason_codes": ",".join(d.get("reason_codes") or []),
        "decision.model_version": str(d.get("model_version", "")),
        "decision.rules_version": str(d.get("rules_version", "")),
        "decision.degraded": "true" if d.get("degraded") else "false",
    }
    for name, value in sorted(ctx.features.items()):
        out[f"feature.{name}"] = fmt_number(float(value))
    counters: dict[str, int] = {}
    for ev in ctx.events[:MAX_EVENTS]:
        kind = str(ev["kind"])
        n = counters.get(kind, 0)
        counters[kind] = n + 1
        payload = ev.get("payload") or {}
        out[f"event.{kind}.{n}.event_ts"] = str(payload.get("event_ts", ev.get("event_ts", "")))
        for key in (
            "amount_minor",
            "currency",
            "mcc",
            "channel",
            "merchant_name",
            "country",
            "ip_prefix",
            "method",
            "success",
            "change",
        ):
            if key in payload:
                v = payload[key]
                out[f"event.{kind}.{n}.{key}"] = (
                    ("true" if v else "false") if isinstance(v, bool) else _text(v)
                )
        if "device_id" in payload:
            out[f"event.{kind}.{n}.device"] = str(payload["device_id"])[:12]  # salted hash prefix only
    return out


def load_prompt(version: str = PROMPT_VERSION) -> str:
    return (repo_root() / "prompts" / f"{version}.md").read_text(encoding="utf-8")


def prompt_hash(system: str) -> str:
    return hashlib.sha256(system.encode("utf-8")).hexdigest()[:12]


def user_message(facts: dict[str, str]) -> str:
    return "Summarize this review case for an analyst. Cite fact keys exactly.\n\n" + factfmt.render(facts)


def fallback_summary(facts: dict[str, str]) -> CaseSummaryOutput:
    """Templated, non-AI fact list used when the providers fail (status `fallback`)."""
    keys = [
        "decision.reason_codes",
        "decision.score",
        "feature.amount_minor",
        "feature.new_device",
        "feature.tx_count_10m",
        "feature.distinct_devices_24h",
    ]
    evidence = []
    for k in keys:
        if k in facts and facts[k] not in ("", "-1"):
            label = k.split(".", 1)[1].replace("_", " ")
            evidence.append({"fact_key": k, "statement": f"{label}: {facts[k]}"})
    if not evidence:
        evidence = [
            {"fact_key": "decision.action", "statement": f"action: {facts.get('decision.action', 'REVIEW')}"}
        ]
    return CaseSummaryOutput.model_validate(
        {
            "pattern": "unclear",
            "evidence": evidence[:6],
            "recommended_checks": ["AI summary unavailable: review the account timeline manually."],
            "confidence": 0.0,
        }
    )


Clock = Callable[[], datetime]


def _validate_output(data: dict[str, Any]) -> None:
    CaseSummaryOutput.model_validate(data)


class SummaryPipeline:
    def __init__(
        self,
        gateway: LLMGateway,
        repo: CaseRepository,
        *,
        clock: Clock | None = None,
        system_prompt: str | None = None,
    ) -> None:
        self.gateway = gateway
        self.repo = repo
        self.clock = clock or (lambda: datetime.now(UTC))
        self.system = system_prompt or load_prompt()

    async def run(self, decision_id: str, *, run_id: str | None = None) -> PipelineResult | None:
        run_id = run_id or str(uuid.uuid4())
        steps: list[StepRecord] = []
        case_id: str | None = None

        async def step(node: str, kind: str, fn: Callable[[], Awaitable[Any]], **meta: Any) -> Any:
            started = self.clock()
            t0 = time.perf_counter()
            try:
                value = await fn()
            except Exception as exc:
                steps.append(
                    StepRecord(
                        run_id,
                        case_id,
                        len(steps) + 1,
                        node,
                        kind,
                        meta.get("model"),
                        meta.get("prompt_version"),
                        0,
                        0,
                        0.0,
                        (time.perf_counter() - t0) * 1000,
                        "error",
                        f"{type(exc).__name__}: {exc}"[:2000],
                        started,
                    )
                )
                raise
            rec = StepRecord(
                run_id,
                case_id,
                len(steps) + 1,
                node,
                kind,
                meta.get("model"),
                meta.get("prompt_version"),
                0,
                0,
                0.0,
                (time.perf_counter() - t0) * 1000,
                "ok",
                None,
                started,
            )
            steps.append(rec)
            return value

        async def load() -> CaseContext | None:
            return await self.repo.load_case(decision_id)

        ctx = await step("load_facts", "compute", load)
        if ctx is None:
            return None
        case_id = ctx.case_id
        for s in steps:
            s.task_id = case_id
        raw_facts = build_facts(ctx)

        async def redact() -> dict[str, str]:
            return {k: redact_text(v) for k, v in raw_facts.items()}

        facts: dict[str, str] = await step("redact", "compute", redact)
        llm_result: LLMResult | None = None
        status: Status
        model_name = "none"
        prompt_version = PROMPT_VERSION
        output: CaseSummaryOutput

        async def summarize() -> LLMResult:
            return await self.gateway.complete(
                "case_summary",
                system=self.system,
                user=user_message(facts),
                schema_name="case_summary_v1",
                schema=case_summary_json_schema(),
                prompt_version=PROMPT_VERSION,
                validate=_validate_output,
            )

        try:
            llm_result = await step("summarize", "llm", summarize, prompt_version=PROMPT_VERSION)
        except GatewayExhaustedError as exc:
            rec = steps[-1]
            rec.error = f"{len(exc.attempts)} attempts failed: " + "; ".join(
                f"{a.provider}:{a.error_kind}" for a in exc.attempts
            )
            output = fallback_summary(facts)
            status = "fallback"
            prompt_version = FALLBACK_PROMPT_VERSION
        else:
            rec = steps[-1]
            rec.model = model_name = llm_result.model
            rec.input_tokens, rec.output_tokens, rec.cost_usd = (
                llm_result.input_tokens,
                llm_result.output_tokens,
                llm_result.cost_usd,
            )
            rec.latency_ms = llm_result.latency_ms
            result_data = llm_result.data

            async def validate() -> CaseSummaryOutput:
                return CaseSummaryOutput.model_validate(result_data)

            try:
                output = await step("validate_schema", "compute", validate)
            except ValidationError:
                output = fallback_summary(facts)
                status = "fallback"
                prompt_version = FALLBACK_PROMPT_VERSION
                model_name = "none"
            else:
                status = "ok"

        async def faithfulness() -> Any:
            return check_faithfulness([(e.fact_key, e.statement) for e in output.evidence], facts)

        checked = await step("check_faithfulness", "compute", faithfulness)
        kept = [
            VerifiedEvidence(fact_key=i.fact_key, statement=i.statement, verified=True) for i in checked.kept
        ]
        if status == "ok" and not kept:
            status = "unfaithful"
        stored = StoredSummary(
            pattern=output.pattern,
            evidence=kept,
            recommended_checks=output.recommended_checks,
            confidence=output.confidence,
            model=model_name,
            prompt_version=prompt_version,
            faithfulness=round(checked.score, 4),
            status=status,
        )
        cost = llm_result.cost_usd if llm_result else 0.0
        result = PipelineResult(
            case_id=ctx.case_id,
            status=status,
            summary=stored,
            steps=steps,
            cost_usd=cost,
            facts=facts,
            prompt_hash=prompt_hash(self.system),
        )

        async def persist() -> None:
            await self.repo.save_summary(result)

        await step("persist", "compute", persist)
        await self.repo.save_steps(steps)
        return result
