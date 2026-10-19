#!/usr/bin/env bash
# `make verify-clean` (spec §19 step 1): clone the committed HEAD into a temporary folder and run
# `make bootstrap && make verify` there with no .env file and no provider keys. Uncommitted changes are not
# included. The clone is left in place for inspection; its path is printed at the end.
set -euo pipefail
cd "$(dirname "$0")/.."
dir=$(mktemp -d "${TMPDIR:-/tmp}/verify-fraud-scoring-service.XXXXXX")
git clone -q . "$dir"
cd "$dir"
echo "verify-clean: $(git rev-parse --short HEAD) in $dir"
env -u ANTHROPIC_API_KEY -u OPENAI_API_KEY bash -c 'make bootstrap && trash .env && make verify'
echo "verify-clean: PASS ($dir)"
