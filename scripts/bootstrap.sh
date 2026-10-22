#!/usr/bin/env bash
# Installs every toolchain dependency the Makefile needs. Idempotent; safe to rerun.
# Same install path locally and in the cloud.
set -euo pipefail
cd "$(dirname "$0")/.."

BIN_DIR="$PWD/.cache/bin"
mkdir -p "$BIN_DIR"
GOBIN="$(go env GOPATH)/bin"
export PATH="$GOBIN:$BIN_DIR:$HOME/.local/bin:$PATH"

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in x86_64 | amd64) GOARCH=amd64 ;; arm64 | aarch64) GOARCH=arm64 ;; *) GOARCH="$ARCH" ;; esac

# Pinned tool versions (bump deliberately; Dependabot does not track these).
TERRAFORM_VERSION=1.16.5
AWS_PROVIDER_VERSION=6.68.0
TFLINT_VERSION=0.63.1
HADOLINT_VERSION=2.15.0
TRIVY_VERSION=0.75.0

log() { printf '\033[36m[bootstrap]\033[0m %s\n' "$*"; }

go_tool() { # name module@version
  if ! command -v "$1" >/dev/null 2>&1; then
    log "go install $2"
    GOFLAGS= go install "$2"
  fi
}

go_tool buf github.com/bufbuild/buf/cmd/buf@v1.74.0
go_tool protoc-gen-go google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
go_tool protoc-gen-go-grpc google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2
go_tool protoc-gen-grpc-gateway github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway@v2.32.0
go_tool protoc-gen-openapiv2 github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-openapiv2@v2.32.0
go_tool ghz github.com/bojand/ghz/cmd/ghz@v0.121.0
go_tool goimports golang.org/x/tools/cmd/goimports@v0.52.0
go_tool golangci-lint github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0
go_tool gosec github.com/securego/gosec/v2/cmd/gosec@v2.29.0
go_tool govulncheck golang.org/x/vuln/cmd/govulncheck@v1.9.0
go_tool actionlint github.com/rhysd/actionlint/cmd/actionlint@v1.7.12
go_tool gitleaks github.com/zricethezav/gitleaks/v8@v8.30.1
go_tool goose github.com/pressly/goose/v3/cmd/goose@v3.29.0

log "uv sync (Python dependencies, locked)"
uv sync --frozen

for t in checkov pre-commit; do
  if ! command -v "$t" >/dev/null 2>&1; then
    log "uv tool install $t"
    uv tool install --quiet "$t" || log "WARN: $t install failed (security/tf-check will report it)"
  fi
done

if [ -f web/package.json ]; then
  log "pnpm install (web)"
  pnpm -C web install --frozen-lockfile
fi

fetch() { # url dest
  curl -fsSL --retry 4 --retry-delay 2 -o "$2" "$1"
}

if ! command -v terraform >/dev/null 2>&1; then
  log "terraform $TERRAFORM_VERSION"
  tmp="$(mktemp -d)"
  fetch "https://releases.hashicorp.com/terraform/${TERRAFORM_VERSION}/terraform_${TERRAFORM_VERSION}_${OS}_${GOARCH}.zip" "$tmp/tf.zip"
  python3 -c "import zipfile,sys; zipfile.ZipFile(sys.argv[1]).extractall(sys.argv[2])" "$tmp/tf.zip" "$BIN_DIR"
  chmod +x "$BIN_DIR/terraform"
  rm -rf "$tmp"
fi

# registry.terraform.io may be unreachable: install the AWS provider into the
# implicit local mirror from releases.hashicorp.com so `terraform init` works offline.
PLUGIN_DIR="$HOME/.terraform.d/plugins/registry.terraform.io/hashicorp/aws/${AWS_PROVIDER_VERSION}/${OS}_${GOARCH}"
if [ ! -d "$PLUGIN_DIR" ] && ! curl -fsS -m 5 -o /dev/null https://registry.terraform.io/.well-known/terraform.json 2>/dev/null; then
  log "terraform-provider-aws $AWS_PROVIDER_VERSION into local mirror"
  mkdir -p "$PLUGIN_DIR"
  tmp="$(mktemp -d)"
  fetch "https://releases.hashicorp.com/terraform-provider-aws/${AWS_PROVIDER_VERSION}/terraform-provider-aws_${AWS_PROVIDER_VERSION}_${OS}_${GOARCH}.zip" "$tmp/aws.zip"
  python3 -c "import zipfile,sys; zipfile.ZipFile(sys.argv[1]).extractall(sys.argv[2])" "$tmp/aws.zip" "$PLUGIN_DIR"
  chmod +x "$PLUGIN_DIR"/terraform-provider-aws_*
  rm -rf "$tmp"
fi

if ! command -v tflint >/dev/null 2>&1; then
  log "tflint $TFLINT_VERSION"
  tmp="$(mktemp -d)"
  fetch "https://github.com/terraform-linters/tflint/releases/download/v${TFLINT_VERSION}/tflint_${OS}_${GOARCH}.zip" "$tmp/tflint.zip"
  python3 -c "import zipfile,sys; zipfile.ZipFile(sys.argv[1]).extractall(sys.argv[2])" "$tmp/tflint.zip" "$BIN_DIR"
  chmod +x "$BIN_DIR/tflint"
  rm -rf "$tmp"
fi

if ! command -v hadolint >/dev/null 2>&1; then
  log "hadolint $HADOLINT_VERSION"
  hl_os="$(uname -s)"; hl_arch="$ARCH"; [ "$hl_arch" = "aarch64" ] && hl_arch=arm64
  fetch "https://github.com/hadolint/hadolint/releases/download/v${HADOLINT_VERSION}/hadolint-${hl_os}-${hl_arch}" "$BIN_DIR/hadolint"
  chmod +x "$BIN_DIR/hadolint"
fi

if ! command -v trivy >/dev/null 2>&1; then
  log "trivy $TRIVY_VERSION"
  tr_os="Linux"; [ "$OS" = "darwin" ] && tr_os="macOS"
  tr_arch="64bit"; [ "$GOARCH" = "arm64" ] && tr_arch="ARM64"
  tmp="$(mktemp -d)"
  fetch "https://github.com/aquasecurity/trivy/releases/download/v${TRIVY_VERSION}/trivy_${TRIVY_VERSION}_${tr_os}-${tr_arch}.tar.gz" "$tmp/trivy.tgz"
  tar -xzf "$tmp/trivy.tgz" -C "$BIN_DIR" trivy
  rm -rf "$tmp"
fi

log "done"
