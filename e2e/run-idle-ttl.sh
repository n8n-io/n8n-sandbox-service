#!/usr/bin/env bash
# Same stack as run.sh but enables short API idle TTL and runs only the idle
# specs: tests/sandbox-idle-ttl.spec.ts and tests/sandbox-ephemeral.spec.ts.
# Do not combine idle TTL with the full suite: short stop/delete windows can stop
# or delete a sandbox between a test's requests.
set -euo pipefail
export E2E_IDLE_TTL_SUITE=1
exec "$(cd "$(dirname "$0")" && pwd)/run.sh" "$@"
