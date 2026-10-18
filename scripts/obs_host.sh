#!/usr/bin/env bash
# Host run of the `observability` profile against the seed_host.sh stack (T-41 evidence): the collector,
# Jaeger, Prometheus and Grafana from compose.yaml's images on host networking, with their configs pointed at
# 127.0.0.1 (collector 18580/18581, Prometheus 18591, Grafana 18590, Jaeger UI 16686), then FULL=1
# seed_host.sh with tracing on, then scripts/obs_verify.py. Containers are named obs-*; remove them with
#   docker rm -f obs-collector obs-jaeger obs-prometheus obs-grafana
set -euo pipefail
cd "$(dirname "$0")/.."
C=${OBS_HOST_DIR:-$PWD/artifacts/obs-host}
mkdir -p "$C/prometheus" "$C/grafana/datasources" "$C/grafana/dashboards"
image() { grep -A1 "^  $1:" compose.yaml | sed -n 's/.*image: //p'; }

sed -e 's/0.0.0.0:4317/127.0.0.1:18580/; s/0.0.0.0:4318/127.0.0.1:18581/; s/jaeger:4317/127.0.0.1:4317/; s/0.0.0.0:13133/127.0.0.1:18582/' \
  deploy/otel/collector.yaml > "$C/collector.yaml"
# On host networking Jaeger (itself a collector) already holds the internal telemetry port 8888.
printf '  telemetry:\n    metrics:\n      level: none\n' >> "$C/collector.yaml"
cp deploy/prometheus/alerts.yml "$C/prometheus/"
sed -e 's/scorer:8511/127.0.0.1:18511/; s/feature-updater:8512/127.0.0.1:18512/' \
  -e 's/case-summarizer:8513/127.0.0.1:18513/; s/model-server:8514/127.0.0.1:18514/' \
  deploy/prometheus/prometheus.yml > "$C/prometheus/prometheus.yml"
sed -e 's#http://prometheus:9090#http://127.0.0.1:18591#; s#http://jaeger:16686#http://127.0.0.1:16686#' \
  deploy/grafana/provisioning/datasources/datasources.yaml > "$C/grafana/datasources/datasources.yaml"
cp deploy/grafana/provisioning/dashboards/dashboards.yaml "$C/grafana/dashboards/"

docker rm -f obs-collector obs-jaeger obs-prometheus obs-grafana >/dev/null 2>&1 || true
docker run -d --name obs-jaeger --network host "$(image jaeger)" >/dev/null
docker run -d --name obs-collector --network host -v "$C/collector.yaml:/c.yaml:ro" "$(image otel-collector)" --config=/c.yaml >/dev/null
docker run -d --name obs-prometheus --network host -v "$C/prometheus:/etc/prometheus:ro" "$(image prometheus)" \
  --config.file=/etc/prometheus/prometheus.yml --web.listen-address=127.0.0.1:18591 >/dev/null
docker run -d --name obs-grafana --network host -e GF_SERVER_HTTP_ADDR=127.0.0.1 -e GF_SERVER_HTTP_PORT=18590 \
  -e GF_AUTH_ANONYMOUS_ENABLED=true -e GF_AUTH_ANONYMOUS_ORG_ROLE=Viewer -e GF_SECURITY_DISABLE_INITIAL_ADMIN_CREATION=true \
  -e GF_AUTH_BASIC_ENABLED=false -e GF_AUTH_DISABLE_LOGIN_FORM=true \
  -v "$C/grafana:/etc/grafana/provisioning:ro" -v "$PWD/deploy/grafana/fraud.json:/var/lib/grafana/dashboards/fraud.json:ro" \
  "$(image grafana)" >/dev/null

SCORER_OTLP=http://127.0.0.1:18580 SUMMARIZER_OTLP=http://127.0.0.1:18581 FULL=1 MINCASES=${MINCASES:-30} \
  scripts/seed_host.sh
sleep 15 # two scrape intervals and the span batch timeout
.venv/bin/python scripts/obs_verify.py --prometheus http://127.0.0.1:18591 --grafana http://127.0.0.1:18590 \
  --jaeger http://127.0.0.1:16686
