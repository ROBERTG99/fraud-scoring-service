#!/usr/bin/env bash
# Chaos run (spec §4.7, T-48) on the compose stack after `make up && make seed`: take each dependency down in
# turn, score transactions through the public API, and check that the outage emits its §4.7 signal and that the
# signal clears once the dependency is back. Prints one PASS/FAIL line per outage; exit 0 only if all pass.
#
#   Model server down  → fraud_degraded_total{reason="model"} grows; decisions keep coming
#   Redis down         → fraud_degraded_total{reason="features"} grows
#   Postgres down      → fraud_spool_depth > 0 while down, back to 0 after recovery (replayed)
#   Redpanda down      → fraud_outbox_unsent > 0 while down, back to 0 after recovery
#   MLflow down        → a restarted model server is ready from its cached artifact (/readyz 200)
#   LLM provider down  → covered by tests (fallback summary, fraud_summary_status_total{status="fallback"}):
#                        tests/unit/test_consumer.py::test_provider_failure_writes_fallback
set -euo pipefail
cd "$(dirname "$0")/.."
TOKEN=${FRAUD_SERVICE_TOKEN:-dev-service-token-change-me}
API=${CHAOS_API:-http://127.0.0.1:8511}
NET=$(docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}}{{end}}' "$(docker compose ps -q scorer)")
PROBE_IMAGE=busybox:1.37.0
FAILED=0

log() { printf '\033[36m[chaos]\033[0m %s\n' "$*"; }
result() { # name ok detail
  if [ "$2" = 1 ]; then echo "chaos: PASS $1 ($3)"; else echo "chaos: FAIL $1 ($3)"; FAILED=1; fi
}

# The scorer's admin port is not published (D-052): read it from a throwaway container on the stack network.
metric() { # name [label-filter]
  docker run --rm --network "$NET" "$PROBE_IMAGE" wget -qO- -T 3 http://scorer:8516/metrics 2>/dev/null \
    | awk -v n="$1" -v f="${2:-}" '($1 == n || index($1, n "{") == 1) && index($1, f) { s += $2 } END { printf "%d\n", s + 0 }'
}

# Score N fresh transactions over HTTP; prints how many got a decision (HTTP 200).
score() { # n
  python3 - "$1" "$API" "$TOKEN" <<'PY'
import json, os, sys, time, urllib.request, uuid

n, api, token = int(sys.argv[1]), sys.argv[2], sys.argv[3]
ok = 0
for i in range(n):
    ms = int(time.time() * 1000)
    raw = ms.to_bytes(6, "big") + os.urandom(10)
    u = bytearray(raw)
    u[6] = 0x70 | (u[6] & 0x0F)  # UUIDv7
    u[8] = 0x80 | (u[8] & 0x3F)
    tx = {
        "transaction": {
            "transaction_id": str(uuid.UUID(bytes=bytes(u))),
            "account_id": f"acc_chaos{i % 20:04d}",
            "amount_minor": 2500 + i,
            "currency": "USD",
            "merchant_id": "m_00042",
            "mcc": "5411",
            "channel": "CHANNEL_ECOMMERCE",
            "device_id": "%064x" % (i % 7),
            "ip_prefix": "203.0.113.0/24",
            "country": "US",
        }
    }
    req = urllib.request.Request(
        api + "/v1/score",
        data=json.dumps(tx).encode(),
        headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=5) as resp:
            ok += resp.status == 200
    except Exception:
        pass
print(ok)
PY
}

wait_until() { # seconds command...
  local deadline=$(( $(date +%s) + $1 )); shift
  until "$@"; do
    [ "$(date +%s)" -lt "$deadline" ] || return 1
    sleep 1
  done
}
healthy() { [ "$(docker inspect -f '{{.State.Health.Status}}' "$(docker compose ps -q "$1")")" = healthy ]; }
model_ready() { curl -sf -o /dev/null --max-time 3 http://127.0.0.1:8514/readyz; }

docker compose ps --status running --services | grep -qx scorer || { echo "chaos: start the stack first (make up && make seed)" >&2; exit 2; }
docker pull -q "$PROBE_IMAGE" >/dev/null
model_ready || { echo "chaos: the model server is not ready; run make seed first" >&2; exit 2; }

# ---- model server
log "model server paused"
before=$(metric fraud_degraded_total 'reason="model"')
docker compose pause model-server >/dev/null
ok=$(score 20)
after=$(metric fraud_degraded_total 'reason="model"')
docker compose unpause model-server >/dev/null
result model-server "$(( ok == 20 && after - before >= 20 ))" "$ok of 20 decided, degraded{reason=model} +$(( after - before ))"

# ---- Redis
log "redis stopped"
before=$(metric fraud_degraded_total 'reason="features"')
docker compose stop redis >/dev/null
ok=$(score 20)
after=$(metric fraud_degraded_total 'reason="features"')
docker compose start redis >/dev/null
wait_until 60 healthy redis
result redis "$(( ok == 20 && after - before >= 20 ))" "$ok of 20 decided, degraded{reason=features} +$(( after - before ))"

# ---- Postgres
log "postgres stopped"
docker compose stop postgres >/dev/null
ok=$(score 20)
depth_down=$(metric fraud_spool_depth)
docker compose start postgres >/dev/null
wait_until 60 healthy postgres
drained=0
wait_until 60 bash -c "[ \"\$(docker run --rm --network $NET $PROBE_IMAGE wget -qO- -T 3 http://scorer:8516/metrics | awk '\$1 == \"fraud_spool_depth\" {print \$2+0}')\" = 0 ]" && drained=1
result postgres "$(( ok == 20 && depth_down > 0 && drained == 1 ))" "$ok of 20 decided, spool depth $depth_down while down, drained=$drained"

# ---- Redpanda
log "redpanda stopped"
docker compose stop redpanda >/dev/null
ok=$(score 20)
unsent=0
wait_until 30 bash -c "[ \"\$(docker run --rm --network $NET $PROBE_IMAGE wget -qO- -T 3 http://scorer:8516/metrics | awk '\$1 == \"fraud_outbox_unsent\" {print \$2+0}')\" -gt 0 ]" && unsent=1
docker compose start redpanda >/dev/null
wait_until 90 healthy redpanda
sent=0
wait_until 90 bash -c "[ \"\$(docker run --rm --network $NET $PROBE_IMAGE wget -qO- -T 3 http://scorer:8516/metrics | awk '\$1 == \"fraud_outbox_unsent\" {print \$2+0}')\" = 0 ]" && sent=1
result redpanda "$(( ok == 20 && unsent == 1 && sent == 1 ))" "$ok of 20 decided, outbox unsent while down=$unsent, sent after recovery=$sent"

# ---- MLflow
log "mlflow stopped, model server restarted"
docker compose stop mlflow >/dev/null
docker compose restart model-server >/dev/null
ready=0
wait_until 120 model_ready && ready=1
source=$(curl -sf --max-time 5 -X POST -H 'Content-Type: application/json' -d '{}' http://127.0.0.1:8514/models | python3 -c 'import json,sys; print(json.load(sys.stdin)["loaded"]["production"]["source"])' 2>/dev/null || echo none)
docker compose start mlflow >/dev/null
result mlflow "$(( ready == 1 ))" "model server /readyz after restart without MLflow: ready=$ready, production from $source"

echo "chaos: LLM provider outage is covered by tests/unit/test_consumer.py::test_provider_failure_writes_fallback"
[ "$FAILED" = 0 ] && echo "chaos: all outages emitted their signal" || { echo "chaos: FAILED" >&2; exit 1; }
