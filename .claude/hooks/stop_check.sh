#!/usr/bin/env bash
# Stop: run `make check-fast` before the agent stops; exit 2 feeds failures back.
# Honours stop_hook_active to avoid infinite loops.
set -u
payload="$(cat 2>/dev/null || true)"
active="$(printf '%s' "$payload" | python3 -c 'import json,sys
try: print(str(json.load(sys.stdin).get("stop_hook_active", False)).lower())
except Exception: print("false")' 2>/dev/null || echo false)"
[ "$active" = "true" ] && exit 0
cd "${CLAUDE_PROJECT_DIR:-$(dirname "$0")/../..}" || exit 0
if ! out="$(make check-fast 2>&1)"; then
  printf 'make check-fast failed; fix before stopping:\n%s\n' "$(printf '%s' "$out" | tail -40)" >&2
  exit 2
fi
exit 0
