#!/usr/bin/env bash
# Runs all e2e topologies in sequence: no runner → two runners → single runner (full Playwright suite) → idle TTL.
# Builds Docker images + SDK once up front; phases reuse them (see E2E_SKIP_BUILD in sibling scripts).
# Extra args are passed through to the Playwright invocation in each phase.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
# shellcheck source=e2e/lib/common.sh
source "$SCRIPT_DIR/lib/common.sh"

TLS_DIR="$(mktemp -d)"
API_TLS_DNS="sandbox-api-e2e-mtls"
CONTROL_SANS="runner-control,runner-control-a,runner-control-b"

cleanup() {
	rm -rf "$TLS_DIR"
}
trap cleanup EXIT

echo "Building Docker images (once for all phases)..."
make -C "$PROJECT_DIR" docker-local

echo "Building SDK once for all phases..."
make -C "$PROJECT_DIR" sdk-install sdk-build

echo "Bootstrapping e2e mTLS material once for all phases..."
bash "$PROJECT_DIR/scripts/bootstrap-mtls.sh" \
	--out-dir "$TLS_DIR" \
	--api-san "$API_TLS_DNS" \
	--control-sans "$CONTROL_SANS" \
	--force

export E2E_SKIP_BUILD=1
export E2E_TLS_DIR="$TLS_DIR"
export E2E_API_TLS_DNS="$API_TLS_DNS"

echo "======== E2E 1/3: no-runner (API only) ========"
"$SCRIPT_DIR/run-no-runner.sh" "$@"

echo "Cooling down between e2e phases (host Docker / DinD)..."
sleep 2

echo "======== E2E 2/3: two runners =================="
"$SCRIPT_DIR/run-two-runners.sh" "$@"

echo "Cooling down between e2e phases (host Docker / DinD)..."
sleep 2

echo "======== E2E 3/4: single runner (full suite) =="
"$SCRIPT_DIR/run.sh" "$@"

echo "Cooling down between e2e phases (host Docker / DinD)..."
sleep 2

echo "======== E2E 4/4: idle TTL (dedicated stack) ===="
"$SCRIPT_DIR/run-idle-ttl.sh" "$@"

echo "======== All e2e phases passed ================"
