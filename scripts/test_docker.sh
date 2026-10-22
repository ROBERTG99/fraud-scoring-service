#!/usr/bin/env bash
# `make test-docker` (spec §16): the full test suite (Go with -race, Python, web) inside a container, against
# the compose.test.yaml services (the same project as `make test`, so both can run in one `make verify`; the
# suites use unique databases, Redis prefixes and topics). The runner's exit code is the result.
# `make test-deps-down` removes the services.
set -euo pipefail
cd "$(dirname "$0")/.."
compose=(docker compose -f compose.test.yaml -p fraudtest --profile docker)
"${compose[@]}" build tests
"${compose[@]}" up -d --wait postgres redis redpanda
"${compose[@]}" run --rm -T tests
