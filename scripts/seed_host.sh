#!/usr/bin/env bash
# Host run of the seed path against compose.test.yaml (PG 8521, Redis 8531, Redpanda 8543): fresh database,
# BentoML on artifacts/model, feature-updater, scorer, then `seed`. Used for the D-024 / T-31 measurements.
# Env: RATE, WINDOW, MINCASES, SHADOW (true|false), SCORER_MODEL_URL. Stop the services with:
#   for p in scorer updater model; do kill -TERM -- -$(cat "$SEED_HOST_DIR/$p.pid"); done
set -euo pipefail
cd "$(dirname "$0")/.."
S=${SEED_HOST_DIR:-$PWD/artifacts/seed-host}
mkdir -p "$S/bin"
for b in scorer feature-updater seed; do GOTOOLCHAIN=local go build -o "$S/bin/$b" "./cmd/$b"; done
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
FRAUD_UPDATER_HTTP_ADDR=127.0.0.1:18512 setsid $S/bin/feature-updater > $S/updater.log 2>&1 &
echo $! > $S/updater.pid
FRAUD_GRPC_ADDR=127.0.0.1:18510 FRAUD_HTTP_ADDR=127.0.0.1:18511 FRAUD_MODEL_URL=${SCORER_MODEL_URL:-http://127.0.0.1:18514} \
  FRAUD_SPOOL_DIR=$S/spool FRAUD_RULES_DIR=rules FRAUD_FEATURE_SPEC=features/spec.yaml FRAUD_SHADOW_ENABLED=${SHADOW:-true} \
  setsid $S/bin/scorer > $S/scorer.log 2>&1 &
echo $! > $S/scorer.pid
for i in $(seq 1 180); do
  curl -sf http://127.0.0.1:18514/readyz >/dev/null && curl -sf http://127.0.0.1:18511/readyz >/dev/null && break
  sleep 1
done
curl -sf http://127.0.0.1:18514/readyz && curl -sf http://127.0.0.1:18511/readyz && echo
start=$(date +%s)
FRAUD_MODEL_URL=http://127.0.0.1:18514 $S/bin/seed -data artifacts/data/demo -scorer 127.0.0.1:18510 -min-cases ${MINCASES:-50} ${RATE:+-rate $RATE} ${WINDOW:+-window $WINDOW} 2> $S/seed.log
echo "seed wall time: $(( $(date +%s) - start ))s"
