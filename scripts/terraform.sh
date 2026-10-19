#!/usr/bin/env bash
# terraform from PATH or .cache/bin (make bootstrap). Commands that change real infrastructure or state are
# refused (spec §0 rule 8); no AWS credentials reach terraform from here (validate and test use mock_provider).
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="$PWD/.cache/bin:$PATH"
for arg in "$@"; do
  case "$arg" in
    -*) continue ;;
    apply | destroy | import | taint | untaint | state)
      echo "terraform.sh: '$arg' is not allowed in this repository (spec §0 rule 8)" >&2
      exit 2 ;;
    *) break ;;
  esac
done
command -v terraform >/dev/null || { echo "terraform.sh: terraform not installed; run make bootstrap" >&2; exit 127; }
exec env -u AWS_ACCESS_KEY_ID -u AWS_SECRET_ACCESS_KEY -u AWS_SESSION_TOKEN -u AWS_PROFILE terraform "$@"
