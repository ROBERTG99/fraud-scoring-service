#!/usr/bin/env bash
# PostToolUse: format the file that was just edited. Never fails; a missing formatter is a no-op.
set -u
payload="$(cat 2>/dev/null || true)"
file="$(printf '%s' "$payload" | python3 -c 'import json,sys
try:
    d=json.load(sys.stdin); print((d.get("tool_input") or {}).get("file_path") or "")
except Exception:
    print("")' 2>/dev/null || true)"
[ -n "$file" ] && [ -f "$file" ] || exit 0
GOBIN_DIR="$(go env GOPATH 2>/dev/null)/bin"
export PATH="$PATH:${GOBIN_DIR}:${CLAUDE_PROJECT_DIR:-.}/.cache/bin:$HOME/.local/bin"
have() { command -v "$1" >/dev/null 2>&1; }
case "$file" in
  */gen/*) exit 0 ;;
  *.go)
    if have goimports; then goimports -w -local github.com/ROBERTG99/fraud-scoring-service "$file" >/dev/null 2>&1 || true
    elif have gofmt; then gofmt -w "$file" >/dev/null 2>&1 || true; fi ;;
  *.py)
    if have ruff; then ruff format -q "$file" >/dev/null 2>&1 || true
    elif have uv; then (cd "${CLAUDE_PROJECT_DIR:-.}" && uv run --frozen ruff format -q "$file" >/dev/null 2>&1) || true; fi ;;
  *.ts | *.tsx | *.css | *.json | *.yaml | *.yml)
    root="${CLAUDE_PROJECT_DIR:-.}"
    if [ -x "$root/web/node_modules/.bin/prettier" ]; then "$root/web/node_modules/.bin/prettier" --log-level silent -w "$file" >/dev/null 2>&1 || true; fi ;;
  *.tf | *.tftest.hcl)
    if have terraform; then terraform fmt "$file" >/dev/null 2>&1 || true; fi ;;
  *.proto)
    if have buf; then buf format -w "$file" >/dev/null 2>&1 || true; fi ;;
esac
exit 0
