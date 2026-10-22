---
name: eval-report
description: Regenerate evals/REPORT.md from the latest reports and explain deltas. Use after make eval.
---
# Eval report

1. `make eval` (offline) — writes `evals/reports/*.json` and `evals/REPORT.md` via `scripts/build_report.py`.
2. If a gate failed, run the `eval-analyst` agent on the failing suite.
3. Never edit `evals/REPORT.md` by hand. Live results are added only by `make eval-live`.
4. Update README numbers only from the regenerated report, citing it.
