#!/bin/sh
# Smoke test against local `make up` (compose defaults).
set -eu
SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
export SANDBOX_API_BASE="${SANDBOX_API_BASE:-http://localhost:8080}"
export SANDBOX_API_KEY="${SANDBOX_API_KEY:-test}"
exec "${SCRIPT_DIR}/smoke-sandbox.sh" "$@"
