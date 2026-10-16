"""Recorded LLM responses (cassette.v1): tests/fixtures/llm/<route>/<key>.json.

key = sha256(route | model | prompt_version | canonical_json(messages, schema_name)), canonical JSON with
sorted keys and no whitespace (same layout as evalgate's cassette.v1).
"""

from __future__ import annotations

import hashlib
import json
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from fraud_summarizer.llm.types import ProviderRequest, ProviderResponse


def canonical_json(value: Any) -> str:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False)


def cassette_key(
    route: str, model: str, prompt_version: str, system: str, user: str, schema_name: str
) -> str:
    messages = [{"role": "system", "content": system}, {"role": "user", "content": user}]
    payload = "|".join(
        [route, model, prompt_version, canonical_json({"messages": messages, "schema_name": schema_name})]
    )
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()


class CassetteStore:
    def __init__(self, root: Path) -> None:
        self.root = root

    def path(self, route: str, key: str) -> Path:
        return self.root / route / f"{key}.json"

    def key_for(self, req: ProviderRequest, model: str | None = None) -> str:
        return cassette_key(
            req.route, model or req.model, req.prompt_version, req.system, req.user, req.schema_name
        )

    def get(self, req: ProviderRequest, model: str | None = None) -> ProviderResponse | None:
        p = self.path(req.route, self.key_for(req, model))
        if not p.is_file():
            return None
        doc = json.loads(p.read_text(encoding="utf-8"))
        usage = doc.get("usage", {})
        return ProviderResponse(
            text=doc["response"] if isinstance(doc["response"], str) else canonical_json(doc["response"]),
            model=doc["model"],
            input_tokens=int(usage.get("input_tokens", 0)),
            output_tokens=int(usage.get("output_tokens", 0)),
            stop_reason="end_turn",
            extra={"cassette": str(p), "recorded": bool(doc.get("recorded", False))},
        )

    def put(self, req: ProviderRequest, resp: ProviderResponse, *, recorded: bool = True) -> Path:
        key = self.key_for(req)
        p = self.path(req.route, key)
        p.parent.mkdir(parents=True, exist_ok=True)
        body = {
            "key": key,
            "route": req.route,
            "model": resp.model,
            "prompt_version": req.prompt_version,
            "request_digest": hashlib.sha256(canonical_json([req.system, req.user]).encode()).hexdigest(),
            "response": json.loads(resp.text),
            "usage": {"input_tokens": resp.input_tokens, "output_tokens": resp.output_tokens},
            "recorded_at": datetime.now(UTC).isoformat(),
            "recorded": recorded,
        }
        p.write_text(json.dumps(body, indent=2, sort_keys=True) + "\n", encoding="utf-8")
        return p
