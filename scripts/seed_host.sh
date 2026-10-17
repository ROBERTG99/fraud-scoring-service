#!/usr/bin/env bash
# Host run of the seed path against compose.test.yaml (PG 8521, Redis 8531, Redpanda 8543): fresh database,
# BentoML on artifacts/model, feature-updater, scorer, then `seed`. Used for the D-024 / T-31 measurements.
# Env: RATE, WINDOW, MINCASES, SHADOW (true|false), SCORER_MODEL_URL; SCORER_OTLP / SUMMARIZER_OTLP set each
# service's OTEL_EXPORTER_OTLP_ENDPOINT (gRPC for the scorer, HTTP for the summarizer; scripts/obs_host.sh). FULL=1 also runs the dev-issuer (18515),
# the case-summarizer (18513; SUMMARY_PROVIDER, default fake, so it stays offline) and webserve on web/dist
# (18500): the stack `make e2e` drives with E2E_BASE_URL=http://127.0.0.1:18500. Stop the services with:
#   for p in web summarizer issuer scorer updater model; do kill -TERM -- -$(cat "$SEED_HOST_DIR/$p.pid"); done
set -euo pipefail
cd "$(dirname "$0")/.."
S=${SEED_HOST_DIR:-$PWD/artifacts/seed-host}
mkdir -p "$S/bin"
BINS="scorer feature-updater seed"
[ "${FULL:-}" = 1 ] && BINS="$BINS dev-issuer webserve"
for b in $BINS; do GOTOOLCHAIN=local go build -o "$S/bin/$b" "./cmd/$b"; done
DB=seedhost_$(date +%s)
psql_admin='postgres://fraud:fraud@127.0.0.1:8521/fraud?sslmode=disable'
.venv/bin/python -c "import psycopg;c=psycopg.connect('$psql_admin',autocommit=True);c.execute('CREATE DATABASE $DB')"
export FRAUD_PG_MIGRATE_DSN="postgres://fraud:fraud@127.0.0.1:8521/$DB?sslmode=disable"
export FRAUD_PG_DSN="postgres://fraud_app:fraud_app@127.0.0.1:8521/$DB?sslmode=disable"
export FRAUD_REDIS_ADDR=127.0.0.1:8531 FRAUD_REDIS_PREFIX=$DB FRAUD_KAFKA_BROKERS=127.0.0.1:8543
export FRAUD_SERVICE_TOKEN=seed-host-token
echo "$DB" > $S/db
$S/bin/scorer migrate
# Model server on the trained demo artifact (no MLflow).
BENTOML_DO_NOT_TRACK=true FRAUD_MODEL_DIR=$PWD/artifacts/model FRAUD_MODEL_CACHE=$S/model-cache MLFLOW_TRACKING_URI= \
  setsid .venv/bin/python -m bentoml serve serving.bentoml.service:FraudModel --host 127.0.0.1 --port 18514 > $S/model.log 2>&1 &
echo $! > $S/model.pid
if [ "${FULL:-}" = 1 ]; then
  export FRAUD_ISSUER=http://127.0.0.1:18515 FRAUD_JWKS_URL=http://127.0.0.1:18515/.well-known/jwks.json
  FRAUD_ISSUER_ADDR=127.0.0.1:18515 setsid $S/bin/dev-issuer > $S/issuer.log 2>&1 &
  echo $! > $S/issuer.pid
  FRAUD_SUMMARIZER_PG_DSN=$FRAUD_PG_DSN FRAUD_SUMMARY_PROVIDER=${SUMMARY_PROVIDER:-fake} \
    setsid env ${SUMMARIZER_OTLP:+OTEL_EXPORTER_OTLP_ENDPOINT=$SUMMARIZER_OTLP} .venv/bin/python -m uvicorn fraud_summarizer.app:app \
    --host 127.0.0.1 --port 18513 > $S/summarizer.log 2>&1 &
  echo $! > $S/summarizer.pid
fi
FRAUD_UPDATER_HTTP_ADDR=127.0.0.1:18512 setsid $S/bin/feature-updater > $S/updater.log 2>&1 &
echo $! > $S/updater.pid
FRAUD_GRPC_ADDR=127.0.0.1:18510 FRAUD_HTTP_ADDR=127.0.0.1:18511 FRAUD_MODEL_URL=${SCORER_MODEL_URL:-http://127.0.0.1:18514} \
  FRAUD_SPOOL_DIR=$S/spool FRAUD_RULES_DIR=rules FRAUD_FEATURE_SPEC=features/spec.yaml FRAUD_SHADOW_ENABLED=${SHADOW:-true} \
  setsid env ${SCORER_OTLP:+OTEL_EXPORTER_OTLP_ENDPOINT=$SCORER_OTLP} $S/bin/scorer > $S/scorer.log 2>&1 &
echo $! > $S/scorer.pid
for i in $(seq 1 180); do
  curl -sf http://127.0.0.1:18514/readyz >/dev/null && curl -sf http://127.0.0.1:18511/readyz >/dev/null && break
  sleep 1
done
curl -sf http://127.0.0.1:18514/readyz && curl -sf http://127.0.0.1:18511/readyz && echo
start=$(date +%s)
FRAUD_MODEL_URL=http://127.0.0.1:18514 $S/bin/seed -data artifacts/data/demo -scorer 127.0.0.1:18510 -min-cases ${MINCASES:-50} ${RATE:+-rate $RATE} ${WINDOW:+-window $WINDOW} 2> $S/seed.log
echo "seed wall time: $(( $(date +%s) - start ))s"
.venv/bin/python scripts/seed_report.py --db "$DB"
if [ "${FULL:-}" = 1 ]; then
  pnpm -C web build >/dev/null
  FRAUD_WEB_ADDR=127.0.0.1:18500 FRAUD_WEB_API_URL=http://127.0.0.1:18511 FRAUD_WEB_ISSUER_URL=http://127.0.0.1:18515 \
    FRAUD_WEB_SUMMARIZER_URL=http://127.0.0.1:18513 setsid $S/bin/webserve > $S/web.log 2>&1 &
  echo $! > $S/web.pid
  for i in $(seq 1 30); do curl -sf http://127.0.0.1:18500/healthz >/dev/null && curl -sf http://127.0.0.1:18513/readyz >/dev/null && break; sleep 1; done
  echo "console: http://127.0.0.1:18500"
fi
