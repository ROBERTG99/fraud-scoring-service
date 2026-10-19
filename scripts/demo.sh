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
# On the compose stack (`make up && make seed`, T-43) the model server is paused and unpaused instead.
# Every endpoint can be overridden with the DEMO_* variables below, and DEMO_MODEL_STOP / DEMO_MODEL_START
# replace the stop and start commands.
set -euo pipefail
cd "$(dirname "$0")/.."

S=${SEED_HOST_DIR:-$PWD/artifacts/seed-host}
OUT=${DEMO_DIR:-$PWD/artifacts/demo}
STATE=$OUT/checkpoints.json
# Which stack: "host" (FULL=1 scripts/seed_host.sh) or "compose" (make up && make seed). "auto" picks the host
# stack when it left its state behind, else a running compose scorer.
STACK=${DEMO_STACK:-auto}
if [ "$STACK" = auto ]; then
  if [ -f "$S/db" ]; then
    STACK=host
  elif docker compose ps --status running --services 2>/dev/null | grep -qx scorer; then
    STACK=compose
  fi
fi
case "$STACK" in
  compose) # compose.yaml ports, its Redis prefix and service token; the model server is paused and unpaused
    d_scorer=127.0.0.1:8510 d_http=http://127.0.0.1:8511 d_model=http://127.0.0.1:8514 d_issuer=http://127.0.0.1:8515
    d_summ=http://127.0.0.1:8513 d_console=http://127.0.0.1:8500 d_kafka=127.0.0.1:8540 d_redis=127.0.0.1:8530
    d_token=${FRAUD_SERVICE_TOKEN:-dev-service-token-change-me} d_prefix=fr
    d_stop='docker compose pause model-server' d_start='docker compose unpause model-server' ;;
  host)
    d_scorer=127.0.0.1:18510 d_http=http://127.0.0.1:18511 d_model=http://127.0.0.1:18514 d_issuer=http://127.0.0.1:18515
    d_summ=http://127.0.0.1:18513 d_console=http://127.0.0.1:18500 d_kafka=127.0.0.1:8543 d_redis=127.0.0.1:8531
    d_token=seed-host-token d_prefix=$(cat "$S/db" 2>/dev/null || true)
    # BentoML's workers run in their own sessions: signal the server and its children.
    d_stop='p=$(cat "$SEED_HOST_DIR/model.pid"); kill -STOP "$p" $(pgrep -P "$p")'
    d_start='p=$(cat "$SEED_HOST_DIR/model.pid"); kill -CONT "$p" $(pgrep -P "$p")' ;;
  *)
    if [ -z "${DEMO_REDIS_PREFIX:-}" ]; then
      echo "demo: no running stack: 'make up && make seed', or 'FULL=1 scripts/seed_host.sh' (or set the DEMO_* variables)" >&2
      exit 2
    fi ;;
esac
SCORER=${DEMO_SCORER_GRPC:-${d_scorer:-}}
SCORER_HTTP=${DEMO_SCORER_HTTP:-${d_http:-}}
MODEL_URL=${DEMO_MODEL_URL:-${d_model:-}}
ISSUER=${DEMO_ISSUER_URL:-${d_issuer:-}}
SUMMARIZER=${DEMO_SUMMARIZER_URL:-${d_summ:-}}
CONSOLE=${DEMO_CONSOLE_URL:-${d_console:-}}
export FRAUD_KAFKA_BROKERS=${DEMO_KAFKA_BROKERS:-${d_kafka:-}}
export FRAUD_REDIS_ADDR=${DEMO_REDIS_ADDR:-${d_redis:-}}
export FRAUD_SERVICE_TOKEN=${DEMO_SERVICE_TOKEN:-${d_token:-}}
export FRAUD_REDIS_PREFIX=${DEMO_REDIS_PREFIX:-${d_prefix:-}}
MODEL_STOP=${DEMO_MODEL_STOP:-${d_stop:-}}
MODEL_START=${DEMO_MODEL_START:-${d_start:-}}
export SEED_HOST_DIR=$S

for url in "$SCORER_HTTP/readyz" "$MODEL_URL/readyz" "$ISSUER/.well-known/jwks.json" "$SUMMARIZER/readyz" "$CONSOLE/healthz"; do
  if ! curl -sf -o /dev/null --max-time 5 "$url"; then
    echo "demo: $url is not ready ($STACK stack); start it with 'make up && make seed' or 'FULL=1 scripts/seed_host.sh'" >&2
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
