#!/usr/bin/env bash
# SessionStart (Claude Code on the web only): start Docker and install toolchains.
# Idempotent and synchronous, so tests and linters work as soon as the session starts.
set -uo pipefail
[ "${CLAUDE_CODE_REMOTE:-}" = "true" ] || exit 0
cd "${CLAUDE_PROJECT_DIR:-$(dirname "$0")/../..}" || exit 0

if command -v dockerd >/dev/null 2>&1 && ! docker info >/dev/null 2>&1; then
  mkdir -p /etc/docker
  [ -f /etc/docker/daemon.json ] || echo '{"registry-mirrors":["https://mirror.gcr.io"]}' > /etc/docker/daemon.json
  (nohup dockerd > /tmp/dockerd.log 2>&1 &)
  for _ in $(seq 1 30); do docker info >/dev/null 2>&1 && break; sleep 1; done
fi

./scripts/bootstrap.sh >/tmp/session_start_bootstrap.log 2>&1 || echo "bootstrap failed; see /tmp/session_start_bootstrap.log" >&2

if [ -n "${CLAUDE_ENV_FILE:-}" ]; then
  {
    echo "export PATH=\"$(go env GOPATH)/bin:$PWD/.cache/bin:\$HOME/.local/bin:\$PATH\""
    echo "export PLAYWRIGHT_BROWSERS_PATH=\"${PLAYWRIGHT_BROWSERS_PATH:-/opt/pw-browsers}\""
  } >> "$CLAUDE_ENV_FILE"
fi
exit 0
