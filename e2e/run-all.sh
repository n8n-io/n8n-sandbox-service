#!/usr/bin/env bash
# Runs all e2e topologies in sequence: no runner → two runners → single runner (full Playwright suite).
# Builds Docker images + SDK once up front; phases reuse them (see E2E_SKIP_BUILD in sibling scripts).
# Extra args are passed through to the Playwright invocation in each phase.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

echo "Building Docker images (once for all phases)..."
make -C "$PROJECT_DIR" docker-local

echo "Building SDK once for all phases..."
make -C "$PROJECT_DIR" sdk-install sdk-build

export E2E_SKIP_BUILD=1

echo "======== E2E 1/3: no-runner (API only) ========"
"$SCRIPT_DIR/run-no-runner.sh" "$@"

echo "Cooling down between e2e phases (host Docker / DinD)..."
sleep 15

echo "======== E2E 2/3: two runners =================="
"$SCRIPT_DIR/run-two-runners.sh" "$@"

echo "Cooling down between e2e phases (host Docker / DinD)..."
sleep 15

echo "======== E2E 3/3: single runner (full suite) =="
"$SCRIPT_DIR/run.sh" "$@"

echo "======== All e2e phases passed ================"
