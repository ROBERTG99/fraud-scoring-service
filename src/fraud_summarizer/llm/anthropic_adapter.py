"""Anthropic Messages API adapter (official `anthropic` SDK).

Structured output via `output_config.format` (JSON schema), effort per route, a cached system prompt,
no prefill, no forced tool use. `stop_reason` is checked before reading content.
"""

from __future__ import annotations

from typing import Any, cast

import anthropic
from anthropic.types import MessageParam, OutputConfigParam, TextBlockParam

from fraud_summarizer.llm.types import ProviderError, ProviderRequest, ProviderResponse


class AnthropicProvider:
    name = "anthropic"

    def __init__(self, client: anthropic.AsyncAnthropic | None = None, http_client: Any = None) -> None:
        # The gateway owns retries (Appendix B), so the SDK must not retry on its own.
        self.client = client or anthropic.AsyncAnthropic(max_retries=0, http_client=http_client)

    async def complete(self, req: ProviderRequest) -> ProviderResponse:
        # Stable system prompt first and marked for caching; facts go in the user turn.
        system: list[TextBlockParam] = [
            {"type": "text", "text": req.system, "cache_control": {"type": "ephemeral"}}
        ]
        messages: list[MessageParam] = [{"role": "user", "content": req.user}]
        output_config = cast(
            OutputConfigParam, {"effort": req.effort, "format": {"type": "json_schema", "schema": req.schema}}
        )
        try:
            msg = await self.client.with_options(timeout=req.timeout_s).messages.create(
                model=req.model,
                max_tokens=req.max_tokens,
                system=system,
                messages=messages,
                output_config=output_config,
            )
        except anthropic.APITimeoutError as exc:
            raise ProviderError(f"anthropic timeout: {exc}", retryable=True, kind="timeout") from exc
        except anthropic.APIConnectionError as exc:
            raise ProviderError(
                f"anthropic connection error: {exc}", retryable=True, kind="connection"
            ) from exc
        except anthropic.RateLimitError as exc:
            raise ProviderError.from_status(429, f"anthropic rate limited: {exc.message}") from exc
        except anthropic.APIStatusError as exc:
            raise ProviderError.from_status(
                exc.status_code, f"anthropic {exc.status_code}: {exc.message}"
            ) from exc
        if msg.stop_reason == "refusal":
            category = getattr(getattr(msg, "stop_details", None), "category", None)
            raise ProviderError(f"anthropic refusal (category={category})", kind="refusal")
        if msg.stop_reason == "max_tokens":
            raise ProviderError("anthropic output truncated at max_tokens", kind="max_tokens")
        text = next((b.text for b in msg.content if b.type == "text"), None)
        if text is None:
            raise ProviderError(
                f"anthropic returned no text block (stop_reason={msg.stop_reason})", kind="empty"
            )
        usage = msg.usage
        return ProviderResponse(
            text=text,
            model=msg.model,
            input_tokens=int(usage.input_tokens or 0)
            + int(getattr(usage, "cache_creation_input_tokens", 0) or 0),
            output_tokens=int(usage.output_tokens or 0),
            stop_reason=str(msg.stop_reason),
            cache_read_tokens=int(getattr(usage, "cache_read_input_tokens", 0) or 0),
            cache_write_tokens=int(getattr(usage, "cache_creation_input_tokens", 0) or 0),
            extra={"request_id": getattr(msg, "_request_id", None)},
        )
