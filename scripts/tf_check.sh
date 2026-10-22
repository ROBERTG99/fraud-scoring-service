#!/usr/bin/env bash
# `make tf-check`: fmt, init (no backend), validate, test (mock_provider), tflint and checkov
# over infra/terraform, with no AWS credentials. Each step must pass.
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="$PWD/.cache/bin:$HOME/.local/bin:$PATH"
tf=./scripts/terraform.sh
mod=infra/terraform/modules/sagemaker_training
env_dir=infra/terraform/envs/dev
step() { printf '\033[36m[tf-check]\033[0m %s\n' "$*"; }

step "fmt"
$tf fmt -check -recursive infra/terraform
for d in "$mod" "$env_dir"; do
  step "init + validate $d"
  $tf -chdir="$d" init -backend=false -input=false -no-color >/dev/null
  $tf -chdir="$d" validate -no-color
done
step "test $mod (mock_provider aws)"
$tf -chdir="$mod" test -no-color
step "tflint"
tflint --chdir=infra/terraform --config="$PWD/infra/terraform/.tflint.hcl" --init >/dev/null
tflint --chdir=infra/terraform --config="$PWD/infra/terraform/.tflint.hcl" --recursive
step "checkov"
checkov --quiet --compact --framework terraform -d infra/terraform --skip-download
step "ok"
