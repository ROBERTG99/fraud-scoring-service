#!/usr/bin/env bash
# make bench [BENCH_PROFILE=ci|laptop]: Score latency under constant load (spec §11 `latency` suite).
# Starts the test infrastructure (compose.test.yaml), a fresh database, the model server on artifacts/model
# and the scorer on the host, runs ghz for BENCH_DURATION (default 60s) at the profile's rate after a 10 s
# warm-up, and writes evals/reports/latency.json with cmd/benchreport, which fails over budget:
#   ci     300 rps, p95 < 80 ms (NFR-2, gated in CI)
#   laptop 2000 rps, p95 < 50 ms, p99 < 90 ms (NFR-1)
# Requests: unique transaction ids, 5,000 accounts, 9,000 devices, five merchant categories, three channels
# (scripts/bench/score.json). Services are stopped on exit; logs stay in artifacts/bench/.
set -euo pipefail
cd "$(dirname "$0")/.."
PROFILE=${1:-${BENCH_PROFILE:-ci}}
case "$PROFILE" in
  ci) RPS=300 CONC=50 ;;
  laptop) RPS=2000 CONC=200 ;;
  *) echo "unknown profile $PROFILE (ci|laptop)" >&2; exit 2 ;;
esac
DURATION=${BENCH_DURATION:-60s}
B=$PWD/artifacts/bench
mkdir -p "$B/bin"
export GOTOOLCHAIN=auto
PY=${PY:-.venv/bin/python}

docker compose -f compose.test.yaml -p fraudtest up -d --wait >/dev/null 2>&1
if [ ! -f artifacts/model/model.onnx ]; then
  make gen-data SIZE=demo
  make train
fi
for b in scorer benchreport; do go build -o "$B/bin/$b" "./cmd/$b"; done

DB=bench_$(date +%s)
"$PY" -c "import psycopg;c=psycopg.connect('postgres://fraud:fraud@127.0.0.1:8521/fraud?sslmode=disable',autocommit=True);c.execute('CREATE DATABASE $DB')"
export FRAUD_PG_MIGRATE_DSN="postgres://fraud:fraud@127.0.0.1:8521/$DB?sslmode=disable"
export FRAUD_PG_DSN="postgres://fraud_app:fraud_app@127.0.0.1:8521/$DB?sslmode=disable"
export FRAUD_REDIS_ADDR=127.0.0.1:8531 FRAUD_REDIS_PREFIX=$DB FRAUD_KAFKA_BROKERS=127.0.0.1:8543
export FRAUD_SERVICE_TOKEN=bench-token
export FRAUD_ENV=dev # a local stack: plain-HTTP JWKS and the development audit key (D-045, D-050)
"$B/bin/scorer" migrate >/dev/null

pids=()
cleanup() {
  for p in "${pids[@]}"; do kill -TERM -- "-$p" 2>/dev/null || true; done
}
trap cleanup EXIT

BENTOML_DO_NOT_TRACK=true FRAUD_MODEL_DIR=$PWD/artifacts/model FRAUD_MODEL_CACHE=$B/model-cache MLFLOW_TRACKING_URI= \
  setsid "$PY" -m bentoml serve serving.bentoml.service:FraudModel --host 127.0.0.1 --port 19514 >"$B/model.log" 2>&1 &
pids+=($!)
FRAUD_GRPC_ADDR=127.0.0.1:19510 FRAUD_HTTP_ADDR=127.0.0.1:19511 FRAUD_ADMIN_ADDR=127.0.0.1:19516 FRAUD_MODEL_URL=http://127.0.0.1:19514 \
  FRAUD_SPOOL_DIR=$B/spool FRAUD_RULES_DIR=rules FRAUD_FEATURE_SPEC=features/spec.yaml \
  setsid "$B/bin/scorer" >"$B/scorer.log" 2>&1 &
pids+=($!)
for _ in $(seq 1 180); do
  curl -sf http://127.0.0.1:19514/readyz >/dev/null && curl -sf http://127.0.0.1:19511/readyz >/dev/null && break
  sleep 1
done
curl -sf http://127.0.0.1:19514/readyz >/dev/null && curl -sf http://127.0.0.1:19511/readyz >/dev/null

ghz_run() {
  ghz --insecure --proto proto/fraud/v1/scorer.proto --import-paths proto,proto/third_party \
    --call fraud.v1.Scorer/Score --metadata '{"authorization":"Bearer bench-token"}' \
    --data-file scripts/bench/score.json --rps "$RPS" --concurrency "$CONC" --connections 4 \
    --duration-stop wait "$@" 127.0.0.1:19510
}
ghz_run --duration 10s >/dev/null # warm-up: connections, Lua scripts, model server workers
START=$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)
ghz_run --duration "$DURATION" --format json --output "$B/ghz.json"
END=$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)
sleep 2 # let the group-commit writer flush the last batch
# Degraded decisions answer fast by construction: count them in the measured window (benchreport fails > 5%).
read -r DECISIONS DEGRADED < <("$PY" -c "
import psycopg
with psycopg.connect('$FRAUD_PG_MIGRATE_DSN') as c:
    n, d = c.execute('SELECT count(*), count(*) FILTER (WHERE degraded) FROM decisions WHERE decided_at BETWEEN %s AND %s',
                     ('$START', '$END')).fetchone()
print(n, d)")
"$B/bin/benchreport" -in "$B/ghz.json" -out evals/reports/latency.json -profile "$PROFILE" \
  -decisions "$DECISIONS" -degraded "$DEGRADED"
