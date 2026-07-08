#!/bin/sh
# Backward-compatible wrapper for deployed environments (dev/stage/prod).
# Prefer: SMOKE_ENV=dev sh scripts/smoke-sandbox.sh
set -eu
SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
export SMOKE_ENV_FILE="${SMOKE_ENV_FILE:-${SMOKE_DEV_ENV_FILE:-${SCRIPT_DIR}/smoke-dev-sandbox.env}}"
exec "${SCRIPT_DIR}/smoke-sandbox.sh" "$@"
