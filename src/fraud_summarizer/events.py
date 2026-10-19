"""JSON Schema validation for Kafka messages, shared with internal/stream/validate.go."""

from __future__ import annotations

import json
from functools import lru_cache
from typing import Any

from jsonschema import Draft202012Validator

from fraud_summarizer.paths import repo_root

TOPIC_SCHEMA = {
    "events.login": "fraud.event.login.v1",
    "events.device": "fraud.event.device.v1",
    "events.transaction": "fraud.event.transaction.v1",
    "decisions": "fraud.decision.v1",
    "events.dlq": "fraud.dlq.v1",
}

_SCHEMA_FILES = {
    "fraud.event.login.v1": "login.json",
    "fraud.event.device.v1": "device.json",
    "fraud.event.transaction.v1": "transaction.json",
    "fraud.decision.v1": "decision.json",
    "fraud.dlq.v1": "dlq.json",
}


class InvalidEventError(ValueError):
    """Raised when a message does not match its schema."""


@lru_cache(maxsize=1)
def _validators() -> dict[str, Draft202012Validator]:
    base = repo_root() / "schemas" / "events"
    out: dict[str, Draft202012Validator] = {}
    for name, file in _SCHEMA_FILES.items():
        schema = json.loads((base / file).read_text(encoding="utf-8"))
        Draft202012Validator.check_schema(schema)
        out[name] = Draft202012Validator(schema)
    return out


def validate(raw: bytes | str) -> tuple[str, dict[str, Any]]:
    """Validate raw JSON against the schema named in its `schema` field; return (name, document)."""
    try:
        doc = json.loads(raw)
    except (json.JSONDecodeError, UnicodeDecodeError) as exc:
        raise InvalidEventError(f"malformed JSON: {exc}") from exc
    if not isinstance(doc, dict):
        raise InvalidEventError("not a JSON object")
    name = doc.get("schema")
    validator = _validators().get(str(name))
    if validator is None:
        raise InvalidEventError(f"unknown schema {name!r}")
    errors = sorted(validator.iter_errors(doc), key=lambda e: list(e.path))
    if errors:
        raise InvalidEventError("; ".join(f"/{'/'.join(map(str, e.path))}: {e.message}" for e in errors))
    return str(name), doc


def validate_topic(topic: str, raw: bytes | str) -> dict[str, Any]:
    name, doc = validate(raw)
    expected = TOPIC_SCHEMA.get(topic)
    if expected != name:
        raise InvalidEventError(f"topic {topic} expects schema {expected}, got {name}")
    return doc
