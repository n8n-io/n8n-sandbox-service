#!/usr/bin/env bash
# Starts the remote Firecracker runner (runner 2) on a peer VM.
# Invoked by e2e/run-firecracker-two-runners-azure.sh over SSH.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
# shellcheck source=e2e/lib/common.sh
source "$SCRIPT_DIR/lib/common.sh"
export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH

: "${E2E_TLS_DIR:?E2E_TLS_DIR is required}"
: "${E2E_CONTROL_PRIVATE_IP:?E2E_CONTROL_PRIVATE_IP is required}"
: "${E2E_PEER_PRIVATE_IP:?E2E_PEER_PRIVATE_IP is required}"

RUNNER_ADDR="${RUNNER_ADDR:-0.0.0.0:18085}"
RUNNER_CONTROL_LISTEN="${RUNNER_CONTROL_LISTEN:-0.0.0.0:19192}"
RUNNER_CONTROL_ADVERTISE="${RUNNER_CONTROL_ADVERTISE:-runner-control-b:19192}"
API_GRPC_ADDR="${API_GRPC_ADDR:-sandbox-api-e2e-mtls:19094}"
FIRECRACKER_PROXY_PORT_START="${FIRECRACKER_PROXY_PORT_START:-18110}"
RUNNER_INTERNAL_API_KEY="${SANDBOX_API_RUNNER_API_KEY:-runner-test}"
REG_TOKEN="${SANDBOX_API_RUNNER_REGISTRATION_TOKEN:-e2e-reg-token}"
API_TLS_DNS="${E2E_API_TLS_DNS:-sandbox-api-e2e-mtls}"
RUNNER_DATA_DIR="${RUNNER_DATA_DIR:-/var/sandboxes}"
RUNNER_LOG="${RUNNER_LOG:-/tmp/sandbox-runner-firecracker-peer.log}"
RUNNER_ENV_FILE="${RUNNER_ENV_FILE:-$SCRIPT_DIR/.fc-runner-b.env}"
RUNNER_PID=""

cleanup() {
	local exit_code=$?
	if [[ $exit_code -ne 0 && -f "$RUNNER_LOG" ]]; then
		echo "=== Peer runner log ==="
		sudo sed -n '1,240p' "$RUNNER_LOG" || true
	fi
	if [[ -n "$RUNNER_PID" ]]; then
		e2e_stop_supervised_pid "$RUNNER_PID"
	fi
	e2e_kill_tcp_listeners \
		"$(e2e_addr_port "$RUNNER_ADDR")" \
		"$(e2e_addr_port "$RUNNER_CONTROL_LISTEN")"
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

if [[ "${E2E_SKIP_BUILD:-}" != "1" ]]; then
	make -C "$PROJECT_DIR" runner
fi

e2e_install_firecracker_hosts "$E2E_CONTROL_PRIVATE_IP" "$E2E_PEER_PRIVATE_IP"

sudo mkdir -p "$RUNNER_DATA_DIR"
rm -f "$RUNNER_LOG"

e2e_kill_tcp_listeners \
	"$(e2e_addr_port "$RUNNER_ADDR")" \
	"$(e2e_addr_port "$RUNNER_CONTROL_LISTEN")"

runner_id="e2e-firecracker-runner-b-$$"
runner_env=(
	PATH="/usr/local/go/bin:$PATH"
	SANDBOX_RUNNER_BACKEND=firecracker
	SANDBOX_RUNNER_LISTEN_ADDR="$RUNNER_ADDR"
	SANDBOX_RUNNER_DATA_DIR="$RUNNER_DATA_DIR"
	SANDBOX_RUNNER_API_KEYS="$RUNNER_INTERNAL_API_KEY"
	SANDBOX_RUNNER_METRICS_ENABLED=true
	SANDBOX_RUNNER_API_GRPC_ADDR="$API_GRPC_ADDR"
	SANDBOX_RUNNER_REGISTRATION_TOKEN="$REG_TOKEN"
	SANDBOX_RUNNER_REGISTRATION_GRPC_CA_FILE="$E2E_TLS_DIR/runner/ca.crt"
	SANDBOX_RUNNER_REGISTRATION_GRPC_CERT_FILE="$E2E_TLS_DIR/runner/grpc-client.crt"
	SANDBOX_RUNNER_REGISTRATION_GRPC_KEY_FILE="$E2E_TLS_DIR/runner/grpc-client.key"
	SANDBOX_RUNNER_REGISTRATION_GRPC_SERVER_NAME="$API_TLS_DNS"
	SANDBOX_RUNNER_HTTP_BASE_URL="http://${E2E_PEER_PRIVATE_IP}:$(e2e_addr_port "$RUNNER_ADDR")"
	SANDBOX_RUNNER_CONTROL_GRPC_LISTEN_ADDR="$RUNNER_CONTROL_LISTEN"
	SANDBOX_RUNNER_CONTROL_GRPC_ADVERTISE_ADDR="$RUNNER_CONTROL_ADVERTISE"
	SANDBOX_RUNNER_CONTROL_GRPC_TLS_CERT_FILE="$E2E_TLS_DIR/runner/control-grpc-server.crt"
	SANDBOX_RUNNER_CONTROL_GRPC_TLS_KEY_FILE="$E2E_TLS_DIR/runner/control-grpc-server.key"
	SANDBOX_RUNNER_CONTROL_GRPC_TLS_CLIENT_CA_FILE="$E2E_TLS_DIR/runner/ca.crt"
	SANDBOX_RUNNER_ID="$runner_id"
	SANDBOX_RUNNER_CAPACITY_TOTAL="${SANDBOX_RUNNER_CAPACITY_TOTAL:-4}"
	SANDBOX_RUNNER_FIRECRACKER_PROXY_PORT_START="$FIRECRACKER_PROXY_PORT_START"
)

{
	echo "RUNNER_ADDR=$(printf '%q' "$RUNNER_ADDR")"
	echo "RUNNER_LOG=$(printf '%q' "$RUNNER_LOG")"
	echo "RUNNER_BIN=$(printf '%q' "$PROJECT_DIR/bin/runner-firecracker")"
	echo "RUNNER_ENV=("
	for item in "${runner_env[@]}"; do
		printf '  %q\n' "$item"
	done
	echo ")"
} >"$RUNNER_ENV_FILE"

echo "Starting peer Firecracker runner..."
sudo env "${runner_env[@]}" "$PROJECT_DIR/bin/runner-firecracker" >"$RUNNER_LOG" 2>&1 &
RUNNER_PID=$!
echo "export E2E_RUNNER_PID=${RUNNER_PID}" >>"$RUNNER_ENV_FILE"
echo "$RUNNER_PID" >"$SCRIPT_DIR/.fc-runner-b.pid"

wait_for_http "Peer Firecracker runner" "http://127.0.0.1:$(e2e_addr_port "$RUNNER_ADDR")/readyz"
trap - EXIT
echo "Peer runner ready (pid ${RUNNER_PID})."
