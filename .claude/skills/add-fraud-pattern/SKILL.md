---
name: add-fraud-pattern
description: Add a synthetic fraud pattern to the generator and propagate it to evals and the summarizer.
---
# Add a fraud pattern

1. Describe the pattern in `cmd/gen/PATTERNS.md` (signals, rate, label rule).
2. Implement it in `cmd/gen/` behind `GEN_VERSION`; bump the version if outputs change.
3. Update `TestFraudRateWithinTolerance` and the determinism SHA test.
4. Add the pattern to the summarizer `pattern` enum, FakeLLM mapping and eval datasets.
5. Retrain (`train-and-register-model`) and re-run `make eval`.
