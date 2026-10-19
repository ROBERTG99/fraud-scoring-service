#!/usr/bin/env bash
# terraform from PATH or .cache/bin (make bootstrap), for the offline checks only (spec §0 rule 8, D-077):
# an allow-list of subcommands, and no AWS credential source reaches terraform (validate and test use
# mock_provider), so a profile in ~/.aws, SSO, web identity or instance metadata can never be used from here.
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="$PWD/.cache/bin:$PATH"
sub=""
for arg in "$@"; do
  case "$arg" in
    -*) continue ;;
    *) sub=$arg; break ;;
  esac
done
case "$sub" in
  fmt | validate | test | version | providers) ;;
  init)
    case " $* " in
      *" -backend=false "*) ;;
      *) echo "terraform.sh: init only with -backend=false" >&2; exit 2 ;;
    esac ;;
  *)
    echo "terraform.sh: '${sub:-<none>}' is not allowed here; allowed: fmt, validate, test, version, providers, init -backend=false" >&2
    exit 2 ;;
esac
command -v terraform >/dev/null || { echo "terraform.sh: terraform not installed; run make bootstrap" >&2; exit 127; }
exec env -u AWS_ACCESS_KEY_ID -u AWS_SECRET_ACCESS_KEY -u AWS_SESSION_TOKEN -u AWS_PROFILE \
  -u AWS_WEB_IDENTITY_TOKEN_FILE -u AWS_ROLE_ARN -u AWS_ROLE_SESSION_NAME \
  -u AWS_CONTAINER_CREDENTIALS_RELATIVE_URI -u AWS_CONTAINER_CREDENTIALS_FULL_URI -u AWS_CONTAINER_AUTHORIZATION_TOKEN \
  AWS_SHARED_CREDENTIALS_FILE=/dev/null AWS_CONFIG_FILE=/dev/null AWS_EC2_METADATA_DISABLED=true \
  terraform "$@"
