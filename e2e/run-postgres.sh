#!/usr/bin/env bash
# Postgres-backed API e2e (Docker runner). Required phase in run-all.sh.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

export E2E_STORE=postgres

echo "======== Postgres e2e: idle TTL ========="
E2E_IDLE_TTL_SUITE=1 "$SCRIPT_DIR/run-idle-ttl.sh" "$@"

echo "Cooling down between postgres e2e phases..."
sleep 2

echo "======== Postgres e2e: two runners ======="
"$SCRIPT_DIR/run-two-runners.sh" "$@"

echo "Cooling down between postgres e2e phases..."
sleep 2

echo "======== Postgres e2e: multi-pod API ===="
"$SCRIPT_DIR/run-postgres-multi-pod.sh" "$@"

echo "======== Postgres e2e phases passed ======"
