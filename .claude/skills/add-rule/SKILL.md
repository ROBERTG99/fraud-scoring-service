---
name: add-rule
description: Add or change a fraud rule in a new versioned rule set.
---
# Add a rule

1. Copy the active `rules/rules.vYYYY.MM.N.yaml` to a new version; bump `version`.
2. Add the rule: unique `id`, `name`, `when` (expr against the typed feature env), `action`, `reason_code`.
   At most 64 rules.
3. `go test ./internal/rules/...` (schema + compile) and add a golden case in `evals/datasets/decisions`
   via `scripts/gen_eval_sets.py` (dev split).
4. `make eval-decisions`. Hot reload picks up the file; publish `fr:rules:changed` in a running stack.
