"""Summarizer telemetry (spec §14): the Score trace continues from Kafka, steps map to gen_ai.*/app.* spans."""

from __future__ import annotations

from collections.abc import Iterator

import pytest
from opentelemetry import trace
from opentelemetry.sdk.trace import ReadableSpan, TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from prometheus_client import REGISTRY

from fraud_summarizer import telemetry
from fraud_summarizer.consumer import DecisionHandler
from fraud_summarizer.llm.fake import FakeLLM
from tests.unit.test_consumer import DECISION_ID, Processed, decision_msg
from tests.unit.test_pipeline import MemoryRepo, journey_case, pipeline

TRACE_ID = "4bf92f3577b34da6a3ce929d0e0e4736"
TRACEPARENT = f"00-{TRACE_ID}-00f067aa0ba902b7-01"
_EXPORTER = InMemorySpanExporter()


@pytest.fixture
def spans() -> Iterator[InMemorySpanExporter]:
    if not isinstance(trace.get_tracer_provider(), TracerProvider):
        provider = TracerProvider()
        provider.add_span_processor(SimpleSpanProcessor(_EXPORTER))
        trace.set_tracer_provider(provider)
    _EXPORTER.clear()
    yield _EXPORTER
    _EXPORTER.clear()


def by_name(exporter: InMemorySpanExporter) -> dict[str, ReadableSpan]:
    return {s.name: s for s in exporter.get_finished_spans()}


def test_context_from_kafka_headers() -> None:
    ctx = telemetry.context_from_headers([("traceparent", TRACEPARENT.encode()), ("other", b"x")])
    assert format(trace.get_current_span(ctx).get_span_context().trace_id, "032x") == TRACE_ID
    assert not trace.get_current_span(telemetry.context_from_headers(None)).get_span_context().is_valid


async def test_decision_span_continues_the_score_trace(spans: InMemorySpanExporter) -> None:
    repo = MemoryRepo({DECISION_ID: journey_case()})
    handler = DecisionHandler(pipeline(FakeLLM(), repo), Processed())
    result = await handler.handle(decision_msg(), headers=[("traceparent", TRACEPARENT.encode())])
    assert result.outcome == "summarized"
    got = by_name(spans)
    root = got["summarizer.decision"]
    assert format(root.context.trace_id, "032x") == TRACE_ID
    assert root.attributes is not None
    assert (
        root.attributes["app.decision.id"] == DECISION_ID and root.attributes["app.outcome"] == "summarized"
    )
    assert root.attributes["app.summary.status"] == "ok"
    for node in ("load_facts", "redact", "summarize", "validate_schema", "check_faithfulness", "persist"):
        span = got[f"summarizer.{node}"]
        assert span.parent is not None and span.context.trace_id == root.context.trace_id
        assert span.attributes is not None and span.attributes["app.step.node"] == node
        assert span.attributes["app.step.status"] == "ok"
    llm = got["summarizer.summarize"].attributes
    assert llm is not None
    assert llm["app.step.kind"] == "llm" and llm["gen_ai.request.model"] == "fake-case-summary-v1"
    assert llm["gen_ai.usage.input_tokens"] >= 0 and "app.cost_usd" in llm
    assert llm["app.prompt_version"] == "case_summary.v1"
    # The gateway's provider span nests under the summarize step.
    assert got["llm.case_summary"].parent is not None
    assert got["llm.case_summary"].parent.span_id == got["summarizer.summarize"].context.span_id


async def test_llm_cost_is_counted_per_route_and_model(spans: InMemorySpanExporter) -> None:
    labels = {"route": "case_summary", "model": "fake-case-summary-v1"}
    before = REGISTRY.get_sample_value("fraud_llm_cost_usd_total", labels) or 0.0
    calls_before = REGISTRY.get_sample_value("fraud_llm_calls_total", {**labels, "outcome": "ok"}) or 0.0
    repo = MemoryRepo({"d1": journey_case()})
    res = await pipeline(FakeLLM(), repo).run("d1")
    assert res is not None
    after = REGISTRY.get_sample_value("fraud_llm_cost_usd_total", labels)
    assert after is not None and after - before == pytest.approx(res.cost_usd)
    assert REGISTRY.get_sample_value("fraud_llm_calls_total", {**labels, "outcome": "ok"}) == calls_before + 1


def test_json_logs_carry_the_trace_id(spans: InMemorySpanExporter) -> None:
    import json
    import logging

    fmt = telemetry.JsonLogFormatter("case-summarizer")
    record = logging.LogRecord("fraud_summarizer.x", logging.INFO, __file__, 1, "hello %s", ("world",), None)
    record.decision_id = "d1"
    with trace.get_tracer("t").start_as_current_span("op") as span:
        line = json.loads(fmt.format(record))
    assert line["msg"] == "hello world" and line["level"] == "INFO" and line["service"] == "case-summarizer"
    assert (
        line["trace_id"] == format(span.get_span_context().trace_id, "032x") and line["decision_id"] == "d1"
    )
    assert "trace_id" not in json.loads(fmt.format(record))
