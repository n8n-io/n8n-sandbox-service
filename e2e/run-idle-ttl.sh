#!/usr/bin/env bash
# Same stack as run.sh but enables short API idle TTL and runs only tests/sandbox-idle-ttl.spec.ts.
# Do not combine idle TTL with the full suite: short stop/delete windows can race
# long or streaming execs even though receipt-time activity is recorded.
set -euo pipefail
export E2E_IDLE_TTL_SUITE=1
exec "$(cd "$(dirname "$0")" && pwd)/run.sh" "$@"
