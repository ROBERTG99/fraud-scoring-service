---
name: tdd
description: Test-first loop for one change. Use when starting any feature or fix.
---
# TDD loop

1. State the expected behaviour of the change and the decisions it touches (`docs/DECISIONS.md`).
2. Write the failing test first. Run it and confirm it fails for the right reason.
3. Implement the smallest change that makes it pass.
4. Run the area checks: `go test -race ./<pkg>/...` or `uv run pytest <path>` or `pnpm -C web test`.
5. Run `make check-fast`.
6. Note user-visible changes in `CHANGELOG.md`; record any new decision in `docs/DECISIONS.md`.
7. Commit with a Conventional Commit message, no attribution trailers.
