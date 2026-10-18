#!/usr/bin/env bash
# Copy the trained ONNX models into the Triton model repository (profile `triton`).
set -euo pipefail
cd "$(dirname "$0")/.."
prod="${1:-artifacts/model/model.onnx}"
shadow="${2:-$prod}"
# Each file must match the onnx_sha256 training recorded next to it (audit M6 M6, D-047).
verify() {
  meta="$(dirname "$1")/model_meta.json"
  [ -f "$meta" ] || { echo "$1: no model_meta.json next to it, refusing an unverifiable model" >&2; exit 1; }
  want=$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1])).get("onnx_sha256", ""))' "$meta")
  got=$(sha256sum "$1" | cut -d' ' -f1)
  [ -n "$want" ] && [ "$want" = "$got" ] || { echo "$1: sha256 $got does not match $meta ($want)" >&2; exit 1; }
}
verify "$prod"
verify "$shadow"
cp "$prod" serving/triton/model_repository/fraud_gbdt/1/model.onnx
cp "$shadow" serving/triton/model_repository/fraud_gbdt_shadow/1/model.onnx
echo "triton model repository ready"
