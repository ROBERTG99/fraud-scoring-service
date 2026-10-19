---
paths:
  - "src/fraud_summarizer/**"
  - "prompts/**"
  - "config/llm_routes.yaml"
---
# LLM code rules (spec Appendix B)

- Provider SDKs (`anthropic`, `openai`) are imported only in `src/fraud_summarizer/llm/`.
- Model IDs come from `config/llm_routes.yaml` / environment, never from logic. No date suffixes.
- Structured output via JSON schema (`output_config.format`); never force tool use; no assistant prefill.
- Set `output_config.effort` per route; adaptive thinking only where the route asks for it.
- Check `stop_reason` before reading content; handle `refusal` and `max_tokens`.
- Retry only 408, 409, 429, 5xx and connection errors, with backoff and jitter.
- Prompts live in `prompts/<name>.v<N>.md`; every call records prompt version, model, tokens, cost, latency.
- Redact PII before any provider call; validate every output against its schema before persisting.
- The LLM never writes to `decisions` and never affects a decision.
