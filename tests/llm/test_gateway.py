"""LLM gateway, adapters and FakeLLM (spec FR-11, T-21). No network: see conftest.py."""

from __future__ import annotations

import json
import socket
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import anthropic
import httpx2
import openai
import pytest

from fraud_summarizer import facts as factfmt
from fraud_summarizer.llm import (
    GatewayExhaustedError,
    LLMGateway,
    ProviderError,
    ProviderRequest,
    ProviderResponse,
)
from fraud_summarizer.llm.anthropic_adapter import AnthropicProvider
from fraud_summarizer.llm.cassettes import CassetteStore, cassette_key
from fraud_summarizer.llm.fake import FakeLLM
from fraud_summarizer.llm.openai_adapter import OpenAIProvider
from fraud_summarizer.llm.routes import load
from fraud_summarizer.llm.types import Usage
from fraud_summarizer.schemas import CaseSummaryOutput, case_summary_json_schema

FIXTURES = Path(__file__).resolve().parents[1] / "fixtures" / "llm" / "adapters"
FACTS = {
    "decision.action": "REVIEW",
    "decision.score": "0.62",
    "decision.reason_codes": "NEW_DEVICE_AFTER_CREDENTIAL_CHANGE,MODEL_SCORE_ELEVATED",
    "feature.amount_minor": "94000",
    "feature.new_device": "1",
    "feature.minutes_since_email_change": "2",
    "feature.minutes_since_pwd_change": "43200",
    "feature.tx_count_10m": "0",
    "feature.channel": "2",
}


def user_prompt(facts: dict[str, str] = FACTS) -> str:
    return "Summarize the case.\n" + factfmt.render(facts)


@dataclass
class Scripted:
    """Provider that raises or answers from a script."""

    script: list[Any]
    name: str = "scripted"
    calls: list[ProviderRequest] = field(default_factory=list)

    async def complete(self, req: ProviderRequest) -> ProviderResponse:
        self.calls.append(req)
        item = self.script.pop(0)
        if isinstance(item, Exception):
            raise item
        return ProviderResponse(
            text=item, model=req.model, input_tokens=1000, output_tokens=100, stop_reason="end_turn"
        )


def ok_text() -> str:
    return json.dumps(
        {
            "pattern": "unclear",
            "evidence": [{"fact_key": "decision.action", "statement": "Review."}],
            "recommended_checks": ["x"],
            "confidence": 0.5,
        }
    )


def gateway(
    primary: Any, fallback: Any = None, env: dict[str, str] | None = None
) -> tuple[LLMGateway, list[float]]:
    env = {"FRAUD_SUMMARY_PROVIDER": "anthropic"} | (env or {})
    if fallback is not None:
        env["FRAUD_SUMMARY_FALLBACK_PROVIDER"] = "openai"
    sleeps: list[float] = []

    async def sleep(s: float) -> None:
        sleeps.append(s)

    providers = {"anthropic": primary, "openai": fallback or Scripted([])}
    return LLMGateway(load(env=env), providers, sleep=sleep), sleeps


async def run(gw: LLMGateway, user: str | None = None) -> Any:
    return await gw.complete(
        "case_summary",
        system="sys",
        user=user or user_prompt(),
        schema_name="case_summary_v1",
        schema=case_summary_json_schema(),
        prompt_version="case_summary.v1",
        validate=lambda d: CaseSummaryOutput.model_validate(d) and None,
    )


async def test_retries_only_retryable_codes() -> None:
    for status in (408, 409, 429, 500, 503, 529):
        p = Scripted([ProviderError.from_status(status, "x"), ok_text()])
        gw, sleeps = gateway(p)
        res = await run(gw)
        assert len(p.calls) == 2, status
        assert res.attempts[0].status == "error" and res.attempts[1].status == "ok"
        assert len(sleeps) == 1
    for status in (400, 401, 403, 404, 422):
        p = Scripted([ProviderError.from_status(status, "x"), ok_text()])
        gw, sleeps = gateway(p)
        with pytest.raises(GatewayExhaustedError) as exc:
            await run(gw)
        assert len(p.calls) == 1, f"{status} must not be retried"
        assert sleeps == []
        assert exc.value.attempts[0].error_kind == "http"
    # Connection errors and timeouts are retryable; refusals are not.
    p = Scripted(
        [ProviderError("conn", retryable=True, kind="connection"), ProviderError("refused", kind="refusal")]
    )
    gw, _ = gateway(p)
    with pytest.raises(GatewayExhaustedError):
        await run(gw)
    assert len(p.calls) == 2


async def test_three_attempts_then_fallback_provider() -> None:
    primary = Scripted([ProviderError.from_status(529, "overloaded")] * 3)
    fallback = Scripted([ok_text()])
    gw, sleeps = gateway(primary, fallback)
    res = await run(gw)
    assert len(primary.calls) == 3 and len(fallback.calls) == 1
    assert res.provider == "openai" and res.model == "gpt-6-luna"
    assert [a.provider for a in res.attempts] == ["anthropic"] * 3 + ["openai"]
    assert len(sleeps) == 2  # backoff between the three primary attempts only
    assert all(0 < s <= 2.0 for s in sleeps) and sleeps[1] > sleeps[0] * 0.9


async def test_an_unexpected_provider_exception_falls_back_instead_of_escaping() -> None:
    # The anthropic SDK raises TypeError per request when no credentials are configured; any such bug in a
    # provider must end in the fallback or the templated summary, never stall the consumer.
    primary = Scripted([TypeError("Could not resolve authentication method")])
    fallback = Scripted([ok_text()])
    gw, sleeps = gateway(primary, fallback)
    res = await run(gw)
    assert len(primary.calls) == 1 and sleeps == [], "a provider bug is not retried"
    assert res.provider == "openai"
    assert res.attempts[0].error_kind == "provider_error" and "TypeError" in (res.attempts[0].error or "")
    with pytest.raises(GatewayExhaustedError):
        await run(gateway(Scripted([RuntimeError("boom")]))[0])


async def test_a_bug_in_our_validator_is_not_disguised_as_a_provider_failure() -> None:
    # Only the provider call is guarded broadly; a bug in our own parse/validate code must surface, not
    # silently burn the fallback provider and end as a templated summary.
    def broken(_: dict[str, Any]) -> None:
        raise KeyError("pattern")

    primary, fallback = Scripted([ok_text()]), Scripted([ok_text()])
    gw, _ = gateway(primary, fallback)
    with pytest.raises(KeyError):
        await gw.complete(
            "case_summary",
            system="sys",
            user=user_prompt(),
            schema_name="case_summary_v1",
            schema=case_summary_json_schema(),
            prompt_version="case_summary.v1",
            validate=broken,
        )
    assert fallback.calls == []


def test_anthropic_without_credentials_is_unavailable(monkeypatch: pytest.MonkeyPatch) -> None:
    from fraud_summarizer.llm.factory import UnavailableProvider, build_gateway

    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    monkeypatch.delenv("ANTHROPIC_AUTH_TOKEN", raising=False)
    gw = build_gateway(load(env={"FRAUD_SUMMARY_PROVIDER": "anthropic"}))
    assert isinstance(gw.providers["anthropic"], UnavailableProvider)


async def test_three_failures_without_fallback_exhausts() -> None:
    primary = Scripted([ProviderError.from_status(500, "e")] * 3)
    gw, _ = gateway(primary)
    with pytest.raises(GatewayExhaustedError) as exc:
        await run(gw)
    assert len(exc.value.attempts) == 3


async def test_invalid_output_is_not_retried_but_falls_back() -> None:
    bad_schema = json.dumps(
        {"pattern": "phishing", "evidence": [], "recommended_checks": [], "confidence": 2}
    )
    for bad, kind in (("not json", "invalid_json"), (bad_schema, "schema"), ("[1, 2]", "invalid_json")):
        primary = Scripted([bad, ok_text()])
        fallback = Scripted([ok_text()])
        gw, sleeps = gateway(primary, fallback)
        res = await run(gw)
        assert len(primary.calls) == 1, "invalid output must not be retried (Appendix B)"
        assert sleeps == []
        assert [a.error_kind for a in res.attempts] == [kind, None]
        assert res.provider == "openai"


async def test_cost_accounting() -> None:
    p = Scripted([ok_text()])
    gw, _ = gateway(p)
    res = await run(gw)
    # claude-haiku-5-5 at $0.10 / $0.50 per 1M tokens: 1000 in + 100 out = $0.00015
    assert res.cost_usd == pytest.approx(0.00015)
    assert res.input_tokens == 1000 and res.output_tokens == 100
    cfg = load(env={})
    # cache writes cost 1.25x and cache reads 0.1x of the input price
    assert cfg.cost_usd(
        "claude-haiku-5-5", 1000, 100, cache_write_tokens=1000, cache_read_tokens=10_000
    ) == pytest.approx(0.00015 + 0.000125 + 0.0001)


async def test_timeout_is_retryable() -> None:
    import asyncio

    class Slow:
        name = "slow"
        calls = 0

        async def complete(self, req: ProviderRequest) -> ProviderResponse:
            Slow.calls += 1
            if Slow.calls == 1:
                await asyncio.sleep(10)
            return ProviderResponse(ok_text(), req.model, 1, 1, "end_turn")

    gw, _ = gateway(Slow())
    gw.config.routes["case_summary"] = gw.config.routes["case_summary"].__class__(
        **{**gw.config.routes["case_summary"].__dict__, "timeout_s": 0.05}
    )
    res = await run(gw)
    assert res.attempts[0].error_kind == "timeout" and res.attempts[1].status == "ok"


async def test_gateway_redacts_before_provider() -> None:
    p = Scripted([ok_text()])
    gw, _ = gateway(p)
    await run(
        gw,
        user=user_prompt(
            FACTS
            | {
                "event.login.0.note": "contact jane.doe@example.com or +1 415 555 0100, card 4111 1111 1111 1111"
            }
        ),
    )
    sent = p.calls[0].user
    assert "jane.doe@example.com" not in sent and "4111 1111 1111 1111" not in sent and "555 0100" not in sent
    assert "[REDACTED_EMAIL]" in sent and "[REDACTED_CARD]" in sent and "[REDACTED_PHONE]" in sent


def mock_client(cls: Any, responses: dict[str, tuple[int, str]], seen: list[httpx2.Request]) -> Any:
    def handler(request: httpx2.Request) -> httpx2.Response:
        seen.append(request)
        status, name = responses[request.url.path]
        return httpx2.Response(
            status, content=(FIXTURES / name).read_bytes(), headers={"content-type": "application/json"}
        )

    http = httpx2.AsyncClient(transport=httpx2.MockTransport(handler))
    return cls(api_key="test-key", max_retries=0, http_client=http)


def request(model: str) -> ProviderRequest:
    from fraud_summarizer.schemas import provider_schema

    return ProviderRequest(
        route="case_summary",
        model=model,
        system="system prompt",
        user=user_prompt(),
        schema_name="case_summary_v1",
        schema=provider_schema(case_summary_json_schema()),
        max_tokens=2048,
        effort="low",
        timeout_s=8,
        prompt_version="case_summary.v1",
    )


async def test_adapters_against_cassettes() -> None:
    seen: list[httpx2.Request] = []
    client = mock_client(
        anthropic.AsyncAnthropic, {"/v1/messages": (200, "anthropic_messages_ok.json")}, seen
    )
    resp = await AnthropicProvider(client=client).complete(request("claude-haiku-5-5"))
    body = json.loads(seen[0].content)
    assert body["model"] == "claude-haiku-5-5"
    assert body["output_config"]["effort"] == "low"
    assert body["output_config"]["format"]["type"] == "json_schema"
    assert body["system"][0]["cache_control"] == {"type": "ephemeral"}
    assert "thinking" not in body and "tool_choice" not in body and "temperature" not in body
    assert body["messages"][-1]["role"] == "user"  # no assistant prefill
    assert "minLength" not in json.dumps(body["output_config"]["format"]["schema"])
    CaseSummaryOutput.model_validate_json(resp.text)
    assert resp.input_tokens == 1180 and resp.output_tokens == 164 and resp.cache_read_tokens == 512

    for path_file, kind, retryable, status in (
        ("anthropic_messages_refusal.json", "refusal", False, 200),
        ("anthropic_error_429.json", "http", True, 429),
        ("anthropic_error_400.json", "http", False, 400),
    ):
        client = mock_client(anthropic.AsyncAnthropic, {"/v1/messages": (status, path_file)}, [])
        with pytest.raises(ProviderError) as exc:
            await AnthropicProvider(client=client).complete(request("claude-haiku-5-5"))
        assert exc.value.kind == kind and exc.value.retryable is retryable, path_file
        if kind == "refusal":  # billed, though unusable: the usage travels with the error (D-062)
            assert exc.value.usage == Usage("claude-haiku-5-5", 1180, 0)

    seen = []
    oclient = mock_client(openai.AsyncOpenAI, {"/v1/responses": (200, "openai_responses_ok.json")}, seen)
    oresp = await OpenAIProvider(client=oclient).complete(request("gpt-6-luna"))
    obody = json.loads(seen[0].content)
    assert obody["text"]["format"]["type"] == "json_schema" and obody["text"]["format"]["strict"] is True
    assert obody["instructions"] == "system prompt" and obody["model"] == "gpt-6-luna"
    assert CaseSummaryOutput.model_validate_json(oresp.text).pattern == "card_testing"
    oclient = mock_client(openai.AsyncOpenAI, {"/v1/responses": (200, "openai_responses_refusal.json")}, [])
    with pytest.raises(ProviderError) as oexc:
        await OpenAIProvider(client=oclient).complete(request("gpt-6-luna"))
    assert oexc.value.kind == "refusal"
    assert oexc.value.usage == Usage("gpt-6-luna", 1100, 8)


async def test_fake_is_deterministic_and_cassettes_win(tmp_path: Path) -> None:
    fake = FakeLLM()
    req = request("fake-case-summary-v1")
    a, b = await fake.complete(req), await fake.complete(req)
    assert a.text == b.text
    out = CaseSummaryOutput.model_validate_json(a.text)
    assert out.pattern == "account_takeover"
    assert {e.fact_key for e in out.evidence} == {
        "feature.minutes_since_email_change",
        "feature.new_device",
        "feature.amount_minor",
    }
    store = CassetteStore(tmp_path)
    recorded = ProviderResponse(
        text=json.dumps(
            {
                "pattern": "unclear",
                "evidence": [{"fact_key": "decision.action", "statement": "Recorded."}],
                "recommended_checks": ["r"],
                "confidence": 0.1,
            }
        ),
        model="claude-haiku-5-5",
        input_tokens=10,
        output_tokens=5,
        stop_reason="end_turn",
    )
    store.put(request("claude-haiku-5-5"), recorded, recorded=True)
    replay = await FakeLLM(cassettes=store, cassette_model="claude-haiku-5-5").complete(req)
    assert json.loads(replay.text)["evidence"][0]["statement"] == "Recorded."
    assert replay.extra["recorded"] is True
    key = cassette_key(
        "case_summary", "claude-haiku-5-5", "case_summary.v1", req.system, req.user, "case_summary_v1"
    )
    assert (tmp_path / "case_summary" / f"{key}.json").is_file()


def test_routes_resolve_models_from_env() -> None:
    cfg = load(env={})
    r = cfg.routes["case_summary"]
    assert (
        r.provider == "anthropic"
        and r.model_for("anthropic") == "claude-haiku-5-5"
        and r.effort == "low"
        and r.attempts == 3
    )
    assert cfg.routes["judge_usefulness"].model_for("anthropic") == "claude-haiku-5-5"
    cfg = load(env={"FRAUD_SUMMARY_PROVIDER": "fake", "FRAUD_SUMMARY_MODEL": "claude-opus-5-5"})
    assert (
        cfg.routes["case_summary"].provider == "fake"
        and cfg.routes["case_summary"].model_for("anthropic") == "claude-opus-5-5"
    )
    with pytest.raises(ValueError, match="unknown provider"):
        load(env={"FRAUD_SUMMARY_PROVIDER": "llama"})


def test_socket_guard_blocks_network() -> None:
    with pytest.raises(Exception, match=r"(?i)socket"):
        socket.create_connection(("127.0.0.1", 9), timeout=0.1)


async def test_a_spent_daily_budget_stops_paid_calls_until_the_next_utc_day() -> None:
    """Audit M6 M7: a runtime cost cap. Calls that would exceed it fail over to the templated summary."""
    from datetime import UTC, datetime, timedelta

    from fraud_summarizer.llm.budget import DailyBudget

    now = datetime(2026, 10, 18, 23, 59, tzinfo=UTC)
    budget = DailyBudget(0.0002, now=lambda: now)  # more than one call (0.00015), less than two
    p = Scripted([ok_text(), ok_text(), ok_text()])
    gw = LLMGateway(load(env={"FRAUD_SUMMARY_PROVIDER": "anthropic"}), {"anthropic": p}, budget=budget)
    await run(gw)
    await run(gw)  # starts under the cap, ends over it: what a call costs is known only afterwards
    assert budget.spent_usd == pytest.approx(0.0003)
    with pytest.raises(GatewayExhaustedError) as exc:
        await run(gw)
    assert len(p.calls) == 2, "no provider call once the budget is spent"
    assert [a.error_kind for a in exc.value.attempts] == ["budget"]
    now += timedelta(minutes=2)  # a new UTC day
    await run(gw)
    assert len(p.calls) == 3 and budget.spent_usd == pytest.approx(0.00015)


async def test_invalid_output_is_charged_to_the_budget() -> None:
    from fraud_summarizer.llm.budget import DailyBudget

    budget = DailyBudget(1.0)
    p = Scripted(["not json"])
    gw = LLMGateway(load(env={"FRAUD_SUMMARY_PROVIDER": "anthropic"}), {"anthropic": p}, budget=budget)
    with pytest.raises(GatewayExhaustedError):
        await run(gw)
    assert budget.spent_usd == pytest.approx(0.00015), (
        "the provider billed the tokens even if we discard them"
    )


def test_the_daily_budget_comes_from_the_environment(monkeypatch: pytest.MonkeyPatch) -> None:
    from fraud_summarizer.llm.budget import DEFAULT_DAILY_BUDGET_USD, budget_from_env

    monkeypatch.delenv("FRAUD_LLM_DAILY_BUDGET_USD", raising=False)
    assert budget_from_env().limit_usd == DEFAULT_DAILY_BUDGET_USD
    monkeypatch.setenv("FRAUD_LLM_DAILY_BUDGET_USD", "2.5")
    assert budget_from_env().limit_usd == 2.5
    for bad in ("-1", "nan", "inf", "lots"):
        monkeypatch.setenv("FRAUD_LLM_DAILY_BUDGET_USD", bad)
        with pytest.raises(ValueError, match="FRAUD_LLM_DAILY_BUDGET_USD"):
            budget_from_env()


def test_an_unpriced_response_model_is_charged_the_highest_price() -> None:
    """Round 2 (N5): a dated or unknown model id must not cost $0 against the budget."""
    cfg = load(env={})
    top_in = max(p[0] for p in cfg.pricing.values())
    top_out = max(p[1] for p in cfg.pricing.values())
    assert cfg.cost_usd("some-model-2026-10-01", 1000, 100) == pytest.approx(
        (1000 * top_in + 100 * top_out) / 1e6
    )


def test_a_route_with_an_unpriced_model_is_refused() -> None:
    from fraud_summarizer.llm.factory import build_gateway

    with pytest.raises(ValueError, match="pricing"):
        build_gateway(
            load(env={"FRAUD_SUMMARY_PROVIDER": "anthropic", "FRAUD_SUMMARY_MODEL": "unpriced-model"})
        )


async def test_a_timed_out_call_is_charged_an_estimate() -> None:
    """The provider may bill a request we stopped waiting for: charge input length plus max_tokens."""
    from fraud_summarizer.llm.budget import DailyBudget

    budget = DailyBudget(1.0)
    p = Scripted([ProviderError("timed out", retryable=False, kind="timeout")])
    gw = LLMGateway(load(env={"FRAUD_SUMMARY_PROVIDER": "anthropic"}), {"anthropic": p}, budget=budget)
    with pytest.raises(GatewayExhaustedError) as exc:
        await run(gw)
    assert budget.spent_usd > 0.001  # 2,048 output tokens at the route's price, plus the input
    assert exc.value.attempts[0].cost_usd == pytest.approx(budget.spent_usd)


async def test_billed_errors_are_charged_to_the_budget() -> None:
    """Security audit round 3: a response cut at max_tokens (what an injected "write forever" asks for) was
    the most expensive call and cost $0 against the budget."""
    from fraud_summarizer.llm.budget import DailyBudget

    budget = DailyBudget(1.0)
    cut = ProviderError("truncated", kind="max_tokens", usage=Usage("claude-haiku-5-5", 1000, 2048))
    p = Scripted([cut])
    gw = LLMGateway(load(env={"FRAUD_SUMMARY_PROVIDER": "anthropic"}), {"anthropic": p}, budget=budget)
    with pytest.raises(GatewayExhaustedError) as exc:
        await run(gw)
    want = load(env={}).cost_usd("claude-haiku-5-5", 1000, 2048)
    assert budget.spent_usd == pytest.approx(want) and exc.value.attempts[0].cost_usd == pytest.approx(want)


async def test_a_connection_error_is_charged_an_estimate() -> None:
    from fraud_summarizer.llm.budget import DailyBudget

    budget = DailyBudget(1.0)
    p = Scripted([ProviderError("reset", retryable=False, kind="connection")])
    gw = LLMGateway(load(env={"FRAUD_SUMMARY_PROVIDER": "anthropic"}), {"anthropic": p}, budget=budget)
    with pytest.raises(GatewayExhaustedError):
        await run(gw)
    assert budget.spent_usd > 0.001  # the request may have reached the provider


async def test_truncated_responses_carry_their_usage_and_are_charged() -> None:
    """M6 review round 4: Anthropic stop_reason max_tokens and OpenAI status incomplete, from cassettes, carry
    the billed usage (cache writes and reads priced as on success), and the gateway charges it."""
    from fraud_summarizer.llm.budget import DailyBudget

    client = mock_client(
        anthropic.AsyncAnthropic, {"/v1/messages": (200, "anthropic_messages_max_tokens.json")}, []
    )
    with pytest.raises(ProviderError) as exc:
        await AnthropicProvider(client=client).complete(request("claude-haiku-5-5"))
    assert exc.value.kind == "max_tokens" and not exc.value.retryable
    assert exc.value.usage == Usage(
        "claude-haiku-5-5", 1180, 2048, cache_write_tokens=600, cache_read_tokens=512
    )
    oclient = mock_client(
        openai.AsyncOpenAI, {"/v1/responses": (200, "openai_responses_incomplete.json")}, []
    )
    with pytest.raises(ProviderError) as oexc:
        await OpenAIProvider(client=oclient).complete(request("gpt-6-luna"))
    assert oexc.value.kind == "max_tokens" and oexc.value.usage == Usage("gpt-6-luna", 1100, 2048)

    config = load(env={"FRAUD_SUMMARY_PROVIDER": "anthropic"})
    for name, provider in (
        (
            "anthropic",
            AnthropicProvider(
                client=mock_client(
                    anthropic.AsyncAnthropic,
                    {"/v1/messages": (200, "anthropic_messages_max_tokens.json")},
                    [],
                )
            ),
        ),
        (
            "openai",
            OpenAIProvider(
                client=mock_client(
                    openai.AsyncOpenAI, {"/v1/responses": (200, "openai_responses_incomplete.json")}, []
                )
            ),
        ),
    ):
        budget = DailyBudget(1.0)
        cfg = load(env={"FRAUD_SUMMARY_PROVIDER": name})
        gw = LLMGateway(cfg, {name: provider}, budget=budget)
        with pytest.raises(GatewayExhaustedError) as gexc:
            await run(gw)
        model = "claude-haiku-5-5" if name == "anthropic" else "gpt-6-luna"
        once = (
            config.cost_usd(model, 1180, 2048, cache_write_tokens=600, cache_read_tokens=512)
            if name == "anthropic"
            else config.cost_usd(model, 1100, 2048)
        )
        attempts = gexc.value.attempts
        assert once > 0 and all(a.cost_usd == pytest.approx(once) for a in attempts), name
        assert budget.spent_usd == pytest.approx(once * len(attempts)), name


def test_anthropic_cache_writes_are_charged_once() -> None:
    """Security audit round 4 (info): a cache write was counted as input and again as a cache write (2.25x)."""
    import asyncio

    client = mock_client(
        anthropic.AsyncAnthropic, {"/v1/messages": (200, "anthropic_messages_cache_write.json")}, []
    )
    resp = asyncio.run(AnthropicProvider(client=client).complete(request("claude-haiku-5-5")))
    assert (resp.input_tokens, resp.cache_write_tokens, resp.cache_read_tokens) == (1180, 600, 512)


async def test_attempts_record_cache_reads_and_writes() -> None:
    """M6 review round 5: input_tokens counts uncached input only, so cache tokens are recorded apart."""
    for name, attempt in (
        ("anthropic_messages_cache_write.json", 0),
        ("anthropic_messages_max_tokens.json", 0),
    ):
        client = mock_client(anthropic.AsyncAnthropic, {"/v1/messages": (200, name)}, [])
        gw = LLMGateway(
            load(env={"FRAUD_SUMMARY_PROVIDER": "anthropic"}), {"anthropic": AnthropicProvider(client=client)}
        )
        try:
            res = await run(gw)
            attempts = res.attempts
        except GatewayExhaustedError as exc:
            attempts = exc.attempts
        a = attempts[attempt]
        assert (a.input_tokens, a.cache_write_tokens, a.cache_read_tokens) == (1180, 600, 512), name
