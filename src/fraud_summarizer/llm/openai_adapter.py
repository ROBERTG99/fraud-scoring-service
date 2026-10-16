"""OpenAI Responses API adapter (official `openai` SDK), behind the same gateway interface.

Structured output via `text.format` (strict JSON schema). The model ID comes from FRAUD_OPENAI_MODEL
(docs/DECISIONS.md D-010). api.openai.com is unreachable in the build environment, so this adapter is
tested against recorded-shape fixtures only.
"""

from __future__ import annotations

from typing import Any

import openai

from fraud_summarizer.llm.types import ProviderError, ProviderRequest, ProviderResponse


class OpenAIProvider:
    name = "openai"

    def __init__(self, client: openai.AsyncOpenAI | None = None, http_client: Any = None) -> None:
        self.client = client or openai.AsyncOpenAI(max_retries=0, http_client=http_client)

    async def complete(self, req: ProviderRequest) -> ProviderResponse:
        try:
            resp = await self.client.with_options(timeout=req.timeout_s).responses.create(  # type: ignore[call-overload]
                model=req.model,
                instructions=req.system,
                input=req.user,
                max_output_tokens=req.max_tokens,
                reasoning={"effort": req.effort},
                text={
                    "format": {
                        "type": "json_schema",
                        "name": req.schema_name,
                        "schema": req.schema,
                        "strict": True,
                    }
                },
            )
        except openai.APITimeoutError as exc:
            raise ProviderError(f"openai timeout: {exc}", retryable=True, kind="timeout") from exc
        except openai.APIConnectionError as exc:
            raise ProviderError(f"openai connection error: {exc}", retryable=True, kind="connection") from exc
        except openai.RateLimitError as exc:
            raise ProviderError.from_status(429, f"openai rate limited: {exc.message}") from exc
        except openai.APIStatusError as exc:
            raise ProviderError.from_status(
                exc.status_code, f"openai {exc.status_code}: {exc.message}"
            ) from exc
        if getattr(resp, "status", "completed") == "incomplete":
            raise ProviderError("openai output incomplete", kind="max_tokens")
        for item in resp.output or []:
            for part in getattr(item, "content", None) or []:
                if getattr(part, "type", "") == "refusal":
                    raise ProviderError("openai refusal", kind="refusal")
        text = resp.output_text
        if not text:
            raise ProviderError("openai returned no output text", kind="empty")
        usage = resp.usage
        return ProviderResponse(
            text=text,
            model=str(resp.model),
            input_tokens=int(getattr(usage, "input_tokens", 0) or 0),
            output_tokens=int(getattr(usage, "output_tokens", 0) or 0),
            stop_reason=str(getattr(resp, "status", "completed")),
        )
