---
name: add-eval-case
description: Add eval cases to a suite through the generator, never by hand-editing test partitions.
---
# Add an eval case

1. Extend `scripts/gen_eval_sets.py` (dev split first). Test partitions are regenerated, never edited.
2. `uv run python scripts/gen_eval_sets.py` and review the diff.
3. If a test partition changes, record the reason in `docs/DECISIONS.md` and refresh the baseline.
4. `make eval`.
