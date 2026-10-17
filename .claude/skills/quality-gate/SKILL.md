---
name: quality-gate
description: Run the quality gates in order and report pass/fail per gate. Use before a milestone commit or release.
---
# Quality gate

Run, in order, stopping to fix at the first failure:

1. `make check-fast`
2. `make lint`
3. `make typecheck`
4. `make security`
5. `make test`
6. `make eval`
7. `make bench`
8. `make tf-check`

Report a table `gate | result | duration`. Never weaken a test or threshold; document gaps in
`docs/KNOWN_LIMITATIONS.md` instead.
