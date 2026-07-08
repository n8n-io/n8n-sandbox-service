#!/usr/bin/env bash
# Firecracker lane with short API idle TTL; runs only tests/sandbox-idle-ttl.spec.ts.
# Uses a dedicated port set so back-to-back suites on the same VM do not collide.
set -euo pipefail
export E2E_IDLE_TTL_SUITE=1
export PORT=8081
export API_GRPC_ADDR=127.0.0.1:19092
export RUNNER_ADDR=127.0.0.1:18083
export RUNNER_CONTROL_LISTEN_ADDR=127.0.0.1:19093
export RUNNER_CONTROL_ADVERTISE_ADDR=localhost:19093
export FIRECRACKER_PROXY_PORT_START=18120
export API_DATA_DIR=/tmp/n8n-sandbox-api-firecracker-idle-ttl-e2e
export API_LOG=/tmp/sandbox-api-firecracker-idle-ttl-e2e.log
export RUNNER_LOG=/tmp/sandbox-runner-firecracker-idle-ttl-e2e.log
exec "$(cd "$(dirname "$0")" && pwd)/run-firecracker.sh" "$@"
