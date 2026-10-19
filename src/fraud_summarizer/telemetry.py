"""Tracing for the summarizer (spec §14).

Spans are exported over OTLP/HTTP only when `OTEL_EXPORTER_OTLP_ENDPOINT` (or the traces-specific variable)
is set, for example to the `observability` profile's collector; otherwise no provider is installed and the
API's no-op tracer is used. A `decisions` message carries the Score span's W3C `traceparent` as a Kafka
header (written by the scorer's outbox relay), so a summary joins the trace of the decision it explains.
"""

from __future__ import annotations

import json
import logging
import os
import sys
from collections.abc import Callable, Sequence
from datetime import UTC, datetime
from typing import Any

from opentelemetry import trace
from opentelemetry.context import Context
from opentelemetry.trace.propagation.tracecontext import TraceContextTextMapPropagator

_PROPAGATOR = TraceContextTextMapPropagator()

KafkaHeaders = Sequence[tuple[str, bytes]] | None


def tracer() -> trace.Tracer:
    return trace.get_tracer("fraud_summarizer")


def init_tracing(service: str) -> Callable[[], None]:
    """Install an OTLP/HTTP batch exporter when an endpoint is configured; return its shutdown."""
    if not (
        os.environ.get("OTEL_EXPORTER_OTLP_ENDPOINT") or os.environ.get("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
    ):
        return lambda: None
    from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
    from opentelemetry.sdk.resources import Resource
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import BatchSpanProcessor

    provider = TracerProvider(resource=Resource.create({"service.name": service}))
    # The exporter reads endpoint, headers and timeout from the OTEL_EXPORTER_OTLP_* variables.
    provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter()))
    trace.set_tracer_provider(provider)
    return provider.shutdown


def context_from_headers(headers: KafkaHeaders) -> Context:
    """The remote span context of a Kafka message's `traceparent`/`tracestate` headers (empty if absent)."""
    carrier = {
        k.lower(): v.decode("ascii", "replace")
        for k, v in (headers or ())
        if k.lower() in ("traceparent", "tracestate") and v
    }
    return _PROPAGATOR.extract(carrier)


_STANDARD = set(logging.LogRecord("", 0, "", 0, "", None, None).__dict__) | {"message", "asctime"}


class JsonLogFormatter(logging.Formatter):
    """One JSON object per line, like the Go services: time, level, msg, service, extras, trace_id/span_id."""

    def __init__(self, service: str) -> None:
        super().__init__()
        self.service = service

    def format(self, record: logging.LogRecord) -> str:
        doc: dict[str, Any] = {
            "time": datetime.fromtimestamp(record.created, UTC).isoformat(),
            "level": record.levelname,
            "msg": record.getMessage(),
            "service": self.service,
            "logger": record.name,
        }
        doc.update({k: v for k, v in record.__dict__.items() if k not in _STANDARD})
        ctx = trace.get_current_span().get_span_context()
        if ctx.is_valid:
            doc["trace_id"], doc["span_id"] = format(ctx.trace_id, "032x"), format(ctx.span_id, "016x")
        if record.exc_info:
            doc["exc"] = self.formatException(record.exc_info)
        return json.dumps(doc, default=str)


def configure_logging(service: str) -> None:
    """JSON lines on stdout for the root logger (no PII at INFO: callers log ids, never fact values)."""
    handler = logging.StreamHandler(sys.stdout)
    handler.setFormatter(JsonLogFormatter(service))
    root = logging.getLogger()
    root.handlers = [handler]
    root.setLevel(logging.DEBUG if os.environ.get("FRAUD_LOG_LEVEL") == "debug" else logging.INFO)
