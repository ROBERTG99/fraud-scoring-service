---
name: tdd
description: Test-first loop for one work-plan task. Use when starting any task from the work plan.
---
# TDD loop

1. Read the task in `docs/specs/000-build-spec.md` §8 and its FR/NFR acceptance criteria.
2. Write the named "test first" test. Run it and confirm it fails for the right reason.
3. Implement the smallest change that makes it pass.
4. Run the area checks: `go test -race ./<pkg>/...` or `uv run pytest <path>` or `pnpm -C web test`.
5. Run `make check-fast`.
6. Record evidence in `PROGRESS.md`: the command and a one-line result.
7. Commit with a Conventional Commit message, no attribution trailers.
