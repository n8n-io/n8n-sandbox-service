#!/usr/bin/env bash
# Firecracker lane with short API idle TTL; runs only tests/sandbox-idle-ttl.spec.ts.
set -euo pipefail
export E2E_IDLE_TTL_SUITE=1
exec "$(cd "$(dirname "$0")" && pwd)/run-firecracker.sh" "$@"
