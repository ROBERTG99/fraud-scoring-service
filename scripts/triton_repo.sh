#!/usr/bin/env bash
# Copy the trained ONNX models into the Triton model repository (profile `triton`).
set -euo pipefail
cd "$(dirname "$0")/.."
prod="${1:-artifacts/model/model.onnx}"
shadow="${2:-$prod}"
cp "$prod" serving/triton/model_repository/fraud_gbdt/1/model.onnx
cp "$shadow" serving/triton/model_repository/fraud_gbdt_shadow/1/model.onnx
echo "triton model repository ready"
