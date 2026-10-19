# LLM fixtures

- `adapters/*.json` are HTTP response bodies in the documented shape of the Anthropic Messages API and the
  OpenAI Responses API. They were **written by hand from the providers' documentation, not recorded**:
  no provider key was available when this repository was built (see `evals/REPORT.md`). They exercise the
  adapters' request building, parsing and error mapping offline.
- `case_summary/<key>.json` are cassette.v1 files keyed as described in
  `src/fraud_summarizer/llm/cassettes.py`. Files with `"recorded": false` were produced offline (FakeLLM) and
  exist to test cassette replay; `make record-fixtures` (needs `ANTHROPIC_API_KEY`) records real responses
  with `"recorded": true`, which then win over FakeLLM's rules.
