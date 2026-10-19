#!/usr/bin/env bash
# make security (spec §17): every scanner, a pass/fail table, raw reports in artifacts/security/.
# Fails closed: a scanner that finds something, or that cannot run (missing tool, unreachable database),
# fails the target. Accepted findings live in the tools' own mechanisms with a reason next to them
# (justified #nosec / nosec comments, pnpm auditConfig.ignoreGhsas, checkov:skip) and in docs/security.md.
set -uo pipefail
cd "$(dirname "$0")/.."
OUT=artifacts/security
mkdir -p "$OUT"
export GOTOOLCHAIN=auto PATH="$PATH:$HOME/go/bin:$PWD/.cache/bin:$PWD/.venv/bin"
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
# The production binaries and everything they link; internal/testutil (testcontainers, docker client) is
# test-only and no binary imports it (docs/security.md, accepted findings).
run govulncheck govulncheck ./cmd/...
run bandit bandit -q -r src training serving scripts evals -ll
REQ=$(mktemp)
if uv export --frozen --all-groups --no-emit-project --format requirements-txt >"$REQ.all" 2>"$OUT/uv-export.txt" &&
  grep -v -- '-e \|git+https://github.com/ROBERTG99/evalgate' "$REQ.all" >"$REQ" && [ "$(grep -c '==' "$REQ")" -gt 0 ]; then
  run pip-audit pip-audit -r "$REQ" --no-deps --disable-pip --progress-spinner off
else # an empty requirements file would "pass" pip-audit: fail closed instead
  ROWS+=("pip-audit|NOT RUN|uv export failed or listed no packages (see $OUT/uv-export.txt)")
  FAILED=1
fi
rm -f "$REQ.all"
run pnpm-audit pnpm -C web audit --audit-level high
run hadolint hadolint Dockerfile.go Dockerfile.summarizer Dockerfile.test serving/bentoml/Dockerfile deploy/mlflow/Dockerfile deploy/seed/Dockerfile training/Dockerfile
run checkov checkov --quiet --compact -d . --framework dockerfile --skip-path web/node_modules
IMAGES=$(docker compose --profile seed --profile jobs config --images 2>/dev/null | grep '^fraud/' || true)
if [ -z "$IMAGES" ]; then # no image scanned is not a pass
  ROWS+=("trivy|NOT RUN|docker compose config listed no fraud/ images")
  FAILED=1
fi
for img in $IMAGES; do
  tag=${img//[\/:]/-}
  run "trivy-vuln-$tag" trivy image --quiet --scanners vuln --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 "$img"
  # Secrets in our own layers; installed third-party packages are skipped (their docstrings match key
  # patterns, e.g. the Anthropic SDK's '{"type": "service_account"}'); gitleaks covers this repository.
  run "trivy-secret-$tag" trivy image --quiet --scanners secret --skip-dirs /app/.venv --exit-code 1 "$img"
done
run tflint tflint --chdir=infra/terraform --config="$PWD/infra/terraform/.tflint.hcl" --recursive
run actionlint actionlint
run licences .venv/bin/python scripts/check_licenses.py
rm -f "$REQ"

printf '\n%-28s %-8s %s\n' scanner result note
for r in "${ROWS[@]}"; do IFS='|' read -r n s note <<<"$r"; printf '%-28s %-8s %s\n' "$n" "$s" "$note"; done
[ "$FAILED" = 0 ] && echo "security: PASS" || echo "security: FAIL (raw reports in $OUT/; accepted findings in docs/security.md)"
exit "$FAILED"
