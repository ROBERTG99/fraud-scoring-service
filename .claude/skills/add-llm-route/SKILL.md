---
name: add-llm-route
description: Add an LLM route (prompt, model routing, schema, cassette, eval). The summarizer has no agent tools.
---
# Add an LLM route

1. Prompt file `prompts/<name>.v1.md`; facts fenced as data.
2. Route in `config/llm_routes.yaml`: provider env, model, effort, max_tokens, timeout.
3. Pydantic output schema in `src/fraud_summarizer/schemas.py` and JSON Schema mirror in `schemas/`.
4. FakeLLM behaviour in `src/fraud_summarizer/llm/fake.py`; cassette in `tests/fixtures/llm/<route>/`.
5. Tests in `tests/llm/`; eval suite case in `evals/suites/`.
6. Never give the model tools or a write path to decisions.
