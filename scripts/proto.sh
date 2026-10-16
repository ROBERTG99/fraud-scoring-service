#!/usr/bin/env bash
# make proto: buf lint, format check, breaking check, codegen, OpenAPI v3, TypeScript types. Idempotent.
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="$(go env GOPATH)/bin:$PWD/.cache/bin:$PATH"

buf lint
buf format -d --exit-code >/dev/null

if git rev-parse --verify --quiet main >/dev/null && git ls-tree -d --name-only main proto | grep -q proto; then
  buf breaking --against '.git#branch=main'
elif git rev-parse --verify --quiet origin/main >/dev/null && git ls-tree -d --name-only origin/main proto | grep -q proto; then
  buf breaking --against '.git#ref=origin/main'
else
  echo "buf breaking: no baseline (main has no proto/ yet; see docs/DECISIONS.md D-006)"
fi

rm -rf gen/go gen/openapi
buf generate --path proto/fraud
uv run --frozen python scripts/openapi_v3.py gen/openapi/fraud.swagger.json gen/openapi/fraud.yaml
rm -f gen/openapi/fraud.swagger.json

if [ -f web/package.json ] && [ -d web/node_modules ]; then
  pnpm -C web exec openapi-typescript ../gen/openapi/fraud.yaml -o src/api/schema.d.ts >/dev/null
  pnpm -C web exec prettier --log-level warn -w src/api/schema.d.ts >/dev/null
fi
echo "proto: ok"
