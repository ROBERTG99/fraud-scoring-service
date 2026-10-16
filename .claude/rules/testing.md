---
paths:
  - "tests/**"
  - "**/*_test.go"
  - "web/src/**/*.test.ts*"
---
# Testing rules

- Write the failing test first. Name tests after behaviour.
- Fixed seeds, injected clocks, polling with deadlines instead of sleeps, unique topics and Redis prefixes.
- No retries, no skips, no xfail, no weakened thresholds.
- Offline: no API keys, no external network (pytest-socket guard).
