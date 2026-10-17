#!/usr/bin/env bash
# make security (spec §17): every scanner, a pass/fail table, raw reports in artifacts/security/.
# Fails closed: a scanner that finds something, or that cannot run (missing tool, unreachable database),
# fails the target. Accepted findings live in the tools' own mechanisms with a reason next to them
# (justified #nosec / nosec comments, pnpm auditConfig.ignoreGhsas, checkov:skip) and in docs/security.md.
set -uo pipefail
cd "$(dirname "$0")/.."
OUT=artifacts/security
mkdir -p "$OUT"
export GOTOOLCHAIN=local PATH="$PATH:$HOME/go/bin:$PWD/.cache/bin:$PWD/.venv/bin"
declare -a ROWS
FAILED=0

run() { # name, command...
  local name=$1
  shift
  local log="$OUT/$name.txt"
  if "$@" >"$log" 2>&1; then
    ROWS+=("$name|pass|")
  else
    local why
    why=$(grep -m1 -iE "forbidden|no such host|not found|connection refused|command not found" "$log" || true)
    if [ -n "$why" ]; then ROWS+=("$name|NOT RUN|${why:0:90}"); else ROWS+=("$name|FAIL|see $log"); fi
    FAILED=1
  fi
}
skip() { ROWS+=("$1|n/a|$2"); }

run gitleaks gitleaks detect --no-banner --redact --source .
run gosec gosec -quiet ./cmd/... ./internal/...
run govulncheck govulncheck ./cmd/... ./internal/...
run bandit bandit -q -r src training serving scripts evals -ll
REQ=$(mktemp)
uv export --frozen --all-groups --no-emit-project --format requirements-txt 2>/dev/null | grep -v -- '-e \|third_party' >"$REQ"
run pip-audit pip-audit -r "$REQ" --no-deps --disable-pip --progress-spinner off
run pnpm-audit pnpm -C web audit --audit-level high
run hadolint hadolint Dockerfile.go Dockerfile.summarizer serving/bentoml/Dockerfile deploy/mlflow/Dockerfile
run checkov checkov --quiet --compact -d . --framework dockerfile --skip-path web/node_modules --skip-path third_party
for img in $(docker compose config --images 2>/dev/null | grep '^fraud/'); do
  tag=${img//[\/:]/-}
  run "trivy-vuln-$tag" trivy image --quiet --scanners vuln --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 "$img"
  # Secrets in our own layers; installed third-party packages are skipped (their docstrings match key
  # patterns, e.g. the Anthropic SDK's '{"type": "service_account"}'); gitleaks covers this repository.
  run "trivy-secret-$tag" trivy image --quiet --scanners secret --skip-dirs /app/.venv --exit-code 1 "$img"
done
if [ -d infra/terraform ]; then run tflint tflint --chdir infra/terraform; else skip tflint "no infra/terraform yet (T-44)"; fi
if [ -d .github/workflows ]; then run actionlint actionlint; else skip actionlint "no .github/workflows yet (T-45)"; fi
run licences .venv/bin/python scripts/check_licenses.py
rm -f "$REQ"

printf '\n%-28s %-8s %s\n' scanner result note
for r in "${ROWS[@]}"; do IFS='|' read -r n s note <<<"$r"; printf '%-28s %-8s %s\n' "$n" "$s" "$note"; done
[ "$FAILED" = 0 ] && echo "security: PASS" || echo "security: FAIL (raw reports in $OUT/; accepted findings in docs/security.md)"
exit "$FAILED"
