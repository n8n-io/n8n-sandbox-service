#!/usr/bin/env bash
# API + local runner 1 for Firecracker two-runner placement/resilience e2e.
# Runner 2 must already be running on a peer VM (see run-firecracker-two-runners-azure.sh).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
# shellcheck source=e2e/lib/common.sh
source "$SCRIPT_DIR/lib/common.sh"
export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH

if [[ -z "${E2E_RUNNER2_HTTP_ADDR:-}" ]]; then
	echo "Firecracker two-runner e2e requires a peer runner VM." >&2
	echo "Set E2E_RUNNER2_HTTP_ADDR or use e2e/run-firecracker-two-runners-azure.sh." >&2
	exit 1
fi

PORT="${PORT:-18081}"
API_GRPC_ADDR="${API_GRPC_ADDR:-0.0.0.0:19094}"
RUNNER1_ADDR="${RUNNER1_ADDR:-127.0.0.1:18084}"
RUNNER1_CONTROL_LISTEN="${RUNNER1_CONTROL_LISTEN:-0.0.0.0:19191}"
RUNNER1_CONTROL_ADVERTISE="${RUNNER1_CONTROL_ADVERTISE:-runner-control-a:19191}"
FIRECRACKER_PROXY_PORT_START_1="${FIRECRACKER_PROXY_PORT_START_1:-18100}"
API_KEY="${SANDBOX_API_KEY:-test}"
RUNNER_INTERNAL_API_KEY="${SANDBOX_API_RUNNER_API_KEY:-runner-test}"
REG_TOKEN="${SANDBOX_API_RUNNER_REGISTRATION_TOKEN:-e2e-reg-token}"
API_TLS_DNS="${E2E_API_TLS_DNS:-sandbox-api-e2e-mtls}"
TLS_DIR="${E2E_TLS_DIR:?E2E_TLS_DIR is required}"
API_DATA_DIR="${API_DATA_DIR:-/tmp/n8n-sandbox-api-firecracker-2r-e2e}"
RUNNER1_DATA_DIR="${RUNNER1_DATA_DIR:-/var/sandboxes}"
API_LOG="${API_LOG:-/tmp/sandbox-api-firecracker-2r-e2e.log}"
RUNNER1_LOG="${RUNNER1_LOG:-/tmp/sandbox-runner-firecracker-2r-a.log}"
RUNNER1_ENV_FILE="${E2E_RUNNER1_ENV_FILE:-$SCRIPT_DIR/.fc-runner-a.env}"
RUNNER2_ENV_FILE="${E2E_RUNNER2_ENV_FILE:-$SCRIPT_DIR/.fc-runner-b.env}"
API_PID=""
RUNNER1_PID=""

cleanup() {
	local exit_code=$?
	if [[ $exit_code -ne 0 ]]; then
		echo "=== API log ==="
		[[ -f "$API_LOG" ]] && sed -n '1,240p' "$API_LOG" || true
		echo "=== Runner 1 log ==="
		[[ -f "$RUNNER1_LOG" ]] && sed -n '1,240p' "$RUNNER1_LOG" || true
	fi
	if [[ -n "$RUNNER1_PID" ]]; then
		e2e_stop_supervised_pid "$RUNNER1_PID"
	fi
	if [[ -n "$API_PID" ]]; then
		e2e_stop_pid "$API_PID"
	fi
	e2e_kill_tcp_listeners \
		"$(e2e_addr_port "127.0.0.1:${PORT}")" \
		"$(e2e_addr_port "$API_GRPC_ADDR")" \
		"$(e2e_addr_port "$RUNNER1_ADDR")" \
		"$(e2e_addr_port "$RUNNER1_CONTROL_LISTEN")"
	exit "$exit_code"
}
trap cleanup EXIT

wait_for_http() {
	local name=$1 url=$2
	for _ in $(seq 1 60); do
		if curl -sf "$url" >/dev/null 2>&1; then
			echo "${name} is ready."
			return 0
		fi
		sleep 1
	done
	echo "${name} failed to become ready: ${url}" >&2
	return 1
}

wait_for_runners_registered() {
	local metrics_url="http://127.0.0.1:${PORT}/metrics"
	for _ in $(seq 1 90); do
		if curl -sf "$metrics_url" | awk '/^sandbox_runners_registered({[^}]*})?[[:space:]]+2(\.[0-9]+)?([[:space:]]|$)/ { found=1 } END { exit(found ? 0 : 1) }'; then
			echo "Two runners are registered with API."
			return 0
		fi
		sleep 1
	done
	echo "Two runners did not register within 90s" >&2
	return 1
}

if [[ -n "${E2E_CONTROL_PRIVATE_IP:-}" && -n "${E2E_PEER_PRIVATE_IP:-}" ]]; then
	e2e_install_firecracker_hosts "$E2E_CONTROL_PRIVATE_IP" "$E2E_PEER_PRIVATE_IP"
fi

if [[ "${E2E_SKIP_BUILD:-}" != "1" ]]; then
	echo "Building API and runner binaries..."
	make -C "$PROJECT_DIR" api runner
fi

mkdir -p "$API_DATA_DIR"
sudo mkdir -p "$RUNNER1_DATA_DIR"
rm -f "$API_LOG" "$RUNNER1_LOG"

e2e_kill_tcp_listeners \
	"$(e2e_addr_port "127.0.0.1:${PORT}")" \
	"$(e2e_addr_port "$API_GRPC_ADDR")" \
	"$(e2e_addr_port "$RUNNER1_ADDR")" \
	"$(e2e_addr_port "$RUNNER1_CONTROL_LISTEN")"

echo "Starting API host process on port ${PORT}..."
env \
	SANDBOX_API_LISTEN_ADDR="127.0.0.1:${PORT}" \
	SANDBOX_API_GRPC_LISTEN_ADDR="$API_GRPC_ADDR" \
	SANDBOX_API_DATA_DIR="$API_DATA_DIR" \
	SANDBOX_API_KEYS="$API_KEY" \
	SANDBOX_API_METRICS_ENABLED=true \
	SANDBOX_API_RUNNER_REGISTRATION_TOKEN="$REG_TOKEN" \
	SANDBOX_API_RUNNER_API_KEY="$RUNNER_INTERNAL_API_KEY" \
	SANDBOX_API_GRPC_TLS_CERT_FILE="$TLS_DIR/api/grpc-server.crt" \
	SANDBOX_API_GRPC_TLS_KEY_FILE="$TLS_DIR/api/grpc-server.key" \
	SANDBOX_API_GRPC_TLS_CLIENT_CA_FILE="$TLS_DIR/api/ca.crt" \
	SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CA_FILE="$TLS_DIR/api/ca.crt" \
	SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CERT_FILE="$TLS_DIR/api/control-grpc-api-client.crt" \
	SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_KEY_FILE="$TLS_DIR/api/control-grpc-api-client.key" \
	"$PROJECT_DIR/bin/api" >"$API_LOG" 2>&1 &
API_PID=$!

wait_for_http "API" "http://127.0.0.1:${PORT}/healthz"

runner_id="e2e-firecracker-runner-a-$$"
runner_env=(
	PATH="/usr/local/go/bin:$PATH"
	SANDBOX_RUNNER_BACKEND=firecracker
	SANDBOX_RUNNER_LISTEN_ADDR="$RUNNER1_ADDR"
	SANDBOX_RUNNER_DATA_DIR="$RUNNER1_DATA_DIR"
	SANDBOX_RUNNER_API_KEYS="$RUNNER_INTERNAL_API_KEY"
	SANDBOX_RUNNER_METRICS_ENABLED=true
	SANDBOX_RUNNER_API_GRPC_ADDR="sandbox-api-e2e-mtls:$(e2e_addr_port "$API_GRPC_ADDR")"
	SANDBOX_RUNNER_REGISTRATION_TOKEN="$REG_TOKEN"
	SANDBOX_RUNNER_REGISTRATION_GRPC_CA_FILE="$TLS_DIR/runner/ca.crt"
	SANDBOX_RUNNER_REGISTRATION_GRPC_CERT_FILE="$TLS_DIR/runner/grpc-client.crt"
	SANDBOX_RUNNER_REGISTRATION_GRPC_KEY_FILE="$TLS_DIR/runner/grpc-client.key"
	SANDBOX_RUNNER_REGISTRATION_GRPC_SERVER_NAME="$API_TLS_DNS"
	SANDBOX_RUNNER_HTTP_BASE_URL="http://${RUNNER1_ADDR}"
	SANDBOX_RUNNER_CONTROL_GRPC_LISTEN_ADDR="$RUNNER1_CONTROL_LISTEN"
	SANDBOX_RUNNER_CONTROL_GRPC_ADVERTISE_ADDR="$RUNNER1_CONTROL_ADVERTISE"
	SANDBOX_RUNNER_CONTROL_GRPC_TLS_CERT_FILE="$TLS_DIR/runner/control-grpc-server.crt"
	SANDBOX_RUNNER_CONTROL_GRPC_TLS_KEY_FILE="$TLS_DIR/runner/control-grpc-server.key"
	SANDBOX_RUNNER_CONTROL_GRPC_TLS_CLIENT_CA_FILE="$TLS_DIR/runner/ca.crt"
	SANDBOX_RUNNER_ID="$runner_id"
	SANDBOX_RUNNER_CAPACITY_TOTAL="${SANDBOX_RUNNER_CAPACITY_TOTAL:-4}"
	SANDBOX_RUNNER_FIRECRACKER_PROXY_PORT_START="$FIRECRACKER_PROXY_PORT_START_1"
)

{
	echo "RUNNER_ADDR=$(printf '%q' "$RUNNER1_ADDR")"
	echo "RUNNER_LOG=$(printf '%q' "$RUNNER1_LOG")"
	echo "RUNNER_BIN=$(printf '%q' "$PROJECT_DIR/bin/runner-firecracker")"
	echo "RUNNER_ENV=("
	for item in "${runner_env[@]}"; do
		printf '  %q\n' "$item"
	done
	echo ")"
} >"$RUNNER1_ENV_FILE"

echo "Starting Firecracker runner 1..."
sudo env "${runner_env[@]}" "$PROJECT_DIR/bin/runner-firecracker" >"$RUNNER1_LOG" 2>&1 &
RUNNER1_PID=$!
echo "export E2E_RUNNER_PID=${RUNNER1_PID}" >>"$RUNNER1_ENV_FILE"
wait_for_http "Firecracker runner 1" "http://${RUNNER1_ADDR}/readyz"

wait_for_runners_registered
sleep 6

e2e_build_sdk_unless_skip "$PROJECT_DIR"
e2e_install_playwright_deps_if_needed "$SCRIPT_DIR"

export E2E_PROJECT_DIR="$PROJECT_DIR"
export E2E_RUNNER1_HTTP_ADDR="$RUNNER1_ADDR"
export E2E_RUNNER2_HTTP_ADDR
export E2E_RUNNER1_PID="$RUNNER1_PID"
export E2E_RUNNER2_PID="${E2E_RUNNER2_PID:?E2E_RUNNER2_PID is required}"
export E2E_RUNNER1_ENV_FILE="$RUNNER1_ENV_FILE"
export E2E_RUNNER2_ENV_FILE="$RUNNER2_ENV_FILE"
export E2E_RUNNER_INTERNAL_API_KEY="$RUNNER_INTERNAL_API_KEY"

cd "$SCRIPT_DIR"
echo "Running two-runner placement test..."
BASE_URL="http://127.0.0.1:$PORT" SANDBOX_API_KEY="$API_KEY" \
	npx playwright test tests/placement-two-runners.spec.ts "$@"

echo "Running runner failure resilience e2e..."
BASE_URL="http://127.0.0.1:$PORT" SANDBOX_API_KEY="$API_KEY" \
	npx playwright test tests/resilience.spec.ts --grep '@e2e-stopped-runner' "$@"
