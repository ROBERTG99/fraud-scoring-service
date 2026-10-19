---
name: new-adr
description: Create a new Architecture Decision Record. Use for any architectural choice.
---
# New ADR

1. Next number: `ls docs/adr | sort | tail -1`.
2. Create `docs/adr/NNNN-<kebab-title>.md` with sections: Status, Date, Context, Decision,
   Alternatives considered, Consequences.
3. Add a row to `docs/DECISIONS.md` linking the ADR.
4. Commit `docs: add ADR NNNN <title>`.
