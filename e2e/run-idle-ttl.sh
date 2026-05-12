#!/usr/bin/env bash
# Same stack as run.sh but enables short API idle TTL and runs only tests/sandbox-idle-ttl.spec.ts.
# Do not combine idle TTL with the full suite: last_active_at updates when proxied bodies finish,
# so aggressive stop/delete breaks long or streaming execs.
set -euo pipefail
export E2E_IDLE_TTL_SUITE=1
exec "$(cd "$(dirname "$0")" && pwd)/run.sh" "$@"
