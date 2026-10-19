#!/usr/bin/env bash
# The scripted demo (spec §3.4, T-46), offline, against a running stack. Three phases of
# `replay -scenario demo` (cmd/replay/scenario.go) write their checkpoints to artifacts/demo/checkpoints.json:
#   1. journey   - USD 940 from a new device 2 min after an email change → REVIEW, idempotent replay, card
#                  testing burst, invalid event → events.dlq, case in the queue, verified AI summary, shadow
#   2. degraded  - the model server is stopped: decisions continue rules-only, live metrics show it
#   3. recovered - the model server is back and decisions use the model again
# then tests/e2e/demo.spec.ts asserts every checkpoint and walks the analyst journey in the console (open the
# case, highlight evidence, label it fraud). Exit 0 only when every checkpoint is OK.
#
# Stack: the host stack of `FULL=1 scripts/seed_host.sh` (default; its ports, Redis prefix and service token
# are read from $SEED_HOST_DIR). "Stopping" the model server there pauses the server and its workers
# (SIGSTOP) and resumes them (SIGCONT), so the scorer sees it unreachable without a restart (docs/DECISIONS.md D-071).
# Every endpoint can be overridden with the DEMO_* variables below, and DEMO_MODEL_STOP / DEMO_MODEL_START
# replace the stop and start commands (the compose defaults arrive with the compose stack, T-43).
set -euo pipefail
cd "$(dirname "$0")/.."

S=${SEED_HOST_DIR:-$PWD/artifacts/seed-host}
OUT=${DEMO_DIR:-$PWD/artifacts/demo}
STATE=$OUT/checkpoints.json
SCORER=${DEMO_SCORER_GRPC:-127.0.0.1:18510}
SCORER_HTTP=${DEMO_SCORER_HTTP:-http://127.0.0.1:18511}
MODEL_URL=${DEMO_MODEL_URL:-http://127.0.0.1:18514}
ISSUER=${DEMO_ISSUER_URL:-http://127.0.0.1:18515}
SUMMARIZER=${DEMO_SUMMARIZER_URL:-http://127.0.0.1:18513}
CONSOLE=${DEMO_CONSOLE_URL:-http://127.0.0.1:18500}
export FRAUD_KAFKA_BROKERS=${DEMO_KAFKA_BROKERS:-127.0.0.1:8543}
export FRAUD_REDIS_ADDR=${DEMO_REDIS_ADDR:-127.0.0.1:8531}
export FRAUD_SERVICE_TOKEN=${DEMO_SERVICE_TOKEN:-seed-host-token}
if [ -z "${DEMO_REDIS_PREFIX:-}" ] && [ ! -f "$S/db" ]; then
  echo "demo: no running stack: start one with 'FULL=1 scripts/seed_host.sh' (or set the DEMO_* variables)" >&2
  exit 2
fi
export FRAUD_REDIS_PREFIX=${DEMO_REDIS_PREFIX:-$(cat "$S/db")}
# BentoML's workers run in their own sessions: signal the server and its children.
MODEL_STOP=${DEMO_MODEL_STOP:-'p=$(cat "$SEED_HOST_DIR/model.pid"); kill -STOP "$p" $(pgrep -P "$p")'}
MODEL_START=${DEMO_MODEL_START:-'p=$(cat "$SEED_HOST_DIR/model.pid"); kill -CONT "$p" $(pgrep -P "$p")'}
export SEED_HOST_DIR=$S

for url in "$SCORER_HTTP/readyz" "$MODEL_URL/readyz" "$ISSUER/.well-known/jwks.json" "$SUMMARIZER/readyz" "$CONSOLE/healthz"; do
  if ! curl -sf -o /dev/null --max-time 5 "$url"; then
    echo "demo: $url is not ready; start the stack with 'FULL=1 scripts/seed_host.sh'" >&2
    exit 2
  fi
done

mkdir -p "$OUT/bin"
GOTOOLCHAIN=auto go build -o "$OUT/bin/replay" ./cmd/replay
phase() { "$OUT/bin/replay" -scenario demo -phase "$1" -state "$STATE" -scorer "$SCORER" -issuer "$ISSUER" 2>>"$OUT/replay.log"; }

failed=0
echo "demo: phase 1, the journeys"
phase journey || failed=1

echo "demo: phase 2, model server stopped"
model_stopped=1
trap '[ "$model_stopped" = 1 ] && bash -c "$MODEL_START"' EXIT
bash -c "$MODEL_STOP"
phase degraded || failed=1

echo "demo: phase 3, model server started again"
bash -c "$MODEL_START"
model_stopped=0
for _ in $(seq 1 60); do curl -sf -o /dev/null --max-time 2 "$MODEL_URL/readyz" && break; sleep 1; done
phase recovered || failed=1

echo "demo: phase 4, the analyst in the console"
console=$OUT/console.json
DEMO_CHECKPOINTS=$STATE E2E_BASE_URL=$CONSOLE PLAYWRIGHT_JSON_OUTPUT_FILE=$console NODE_PATH=$PWD/web/node_modules \
  pnpm -C web exec playwright test --config ../tests/e2e/playwright.config.ts demo.spec.ts --reporter=json >/dev/null 2>"$OUT/console.log" || failed=1
# One line per console checkpoint, from the Playwright report (titles start with the checkpoint ID).
node -e '
const r = JSON.parse(require("fs").readFileSync(process.argv[1], "utf8"));
const specs = [];
const walk = (s) => { (s.specs || []).forEach((x) => specs.push(x)); (s.suites || []).forEach(walk); };
r.suites.forEach(walk);
const cps = specs.filter((s) => /^[a-z_]+:/.test(s.title));
const before = Number(process.argv[2]), total = before + cps.length;
cps.forEach((s, i) => {
  const ok = s.ok && s.tests.every((t) => t.results.every((x) => x.status === "passed"));
  const [id, ...rest] = s.title.split(": ");
  console.log(`demo: ${ok ? "OK  " : "FAIL"} ${before + i + 1}/${total} ${id} (FR-13, console): ${rest.join(": ")}`);
});
const pre = specs.find((s) => !/^[a-z_]+:/.test(s.title));
if (pre && !pre.ok) console.log(`demo: FAIL ${pre.title} (see artifacts/demo/console.log)`);
' "$console" "$(grep -c '"id"' "$STATE" || true)" | tee "$OUT/console.lines" || failed=1

total=$(( $(grep -c '"id"' "$STATE" || true) + $(grep -c 'console): ' "$OUT/console.lines" || true) ))
ok=$(( $(grep -c '"ok": true' "$STATE" || true) + $(grep -c '^demo: OK .*console): ' "$OUT/console.lines" || true) ))
if [ "$failed" = 0 ] && [ "$ok" = "$total" ]; then
  echo "demo: all $total checkpoints OK (${STATE#"$PWD"/})"
else
  echo "demo: FAILED, $ok of $total checkpoints OK (logs in ${OUT#"$PWD"/})" >&2
  exit 1
fi
