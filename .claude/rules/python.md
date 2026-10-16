---
paths:
  - "**/*.py"
  - "pyproject.toml"
---
# Python rules

- Python 3.12+, run everything with `uv run`. Dependencies are locked in `uv.lock`.
- `mypy --strict` on `src/` and `training/`; ruff for lint and format.
- Pydantic v2 models for every external payload; `extra="forbid"` on inputs we control.
- Async I/O in the summarizer; every network call has a timeout.
- Tests run under pytest-socket: offline by default, loopback only.
