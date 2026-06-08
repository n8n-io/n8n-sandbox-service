#!/usr/bin/env bash
# Runs Firecracker-compatible e2e tests against API and runner host processes.
# Expects the VM to have Firecracker binaries and assets prepared under
# /opt/firecracker, /srv/firecracker, and /srv/jailer.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH

PORT="${PORT:-8080}"
API_GRPC_ADDR="${API_GRPC_ADDR:-127.0.0.1:19090}"
RUNNER_ADDR="${RUNNER_ADDR:-127.0.0.1:18082}"
RUNNER_CONTROL_LISTEN_ADDR="${RUNNER_CONTROL_LISTEN_ADDR:-127.0.0.1:19091}"
RUNNER_CONTROL_ADVERTISE_ADDR="${RUNNER_CONTROL_ADVERTISE_ADDR:-localhost:19091}"
FIRECRACKER_PROXY_PORT_START="${FIRECRACKER_PROXY_PORT_START:-18100}"
API_KEY="${SANDBOX_API_KEY:-test}"
RUNNER_INTERNAL_API_KEY="${SANDBOX_API_RUNNER_API_KEY:-runner-test}"
REG_TOKEN="${SANDBOX_API_RUNNER_REGISTRATION_TOKEN:-e2e-reg-token}"
API_TLS_DNS="${E2E_API_TLS_DNS:-sandbox-api-e2e-mtls}"
TLS_DIR="${E2E_TLS_DIR:-$(mktemp -d)}"
TLS_DIR_OWNED=0
if [[ -z "${E2E_TLS_DIR:-}" ]]; then
	TLS_DIR_OWNED=1
fi

API_DATA_DIR="${API_DATA_DIR:-/tmp/n8n-sandbox-api-firecracker-e2e}"
RUNNER_DATA_DIR="${RUNNER_DATA_DIR:-/var/sandboxes}"
API_LOG="${API_LOG:-/tmp/sandbox-api-firecracker-e2e.log}"
RUNNER_LOG="${RUNNER_LOG:-/tmp/sandbox-runner-firecracker-e2e.log}"
API_PID=""
RUNNER_PID=""

cleanup() {
	local exit_code=$?
	if [[ $exit_code -ne 0 ]]; then
		echo "=== API log ==="
		if [[ -f "$API_LOG" ]]; then
			sed -n '1,240p' "$API_LOG" || true
		fi
		echo "=== Runner log ==="
		if [[ -f "$RUNNER_LOG" ]]; then
			sudo sed -n '1,320p' "$RUNNER_LOG" || true
		fi
	fi

	echo "Stopping Firecracker e2e resources..."
	if [[ -n "$RUNNER_PID" ]]; then
		sudo kill "$RUNNER_PID" >/dev/null 2>&1 || true
		wait "$RUNNER_PID" >/dev/null 2>&1 || true
	fi
	if [[ -n "$API_PID" ]]; then
		kill "$API_PID" >/dev/null 2>&1 || true
		wait "$API_PID" >/dev/null 2>&1 || true
	fi
	sudo rm -rf /srv/jailer/firecracker/sandbox-* >/dev/null 2>&1 || true
	sudo ip netns list | awk '{print $1}' | grep '^fc-sb-' | xargs -r -n1 sudo ip netns delete || true
	if [[ "$TLS_DIR_OWNED" == "1" ]]; then
		rm -rf "$TLS_DIR"
	fi
	exit "$exit_code"
}
trap cleanup EXIT

wait_for_http() {
	local name=$1 url=$2
	for i in $(seq 1 60); do
		if curl -sf "$url" >/dev/null 2>&1; then
			echo "${name} is ready."
			return 0
		fi
		sleep 1
	done
	echo "${name} failed to become ready: ${url}" >&2
	return 1
}

wait_for_runner_registered() {
	local metrics_url="http://127.0.0.1:${PORT}/metrics"
	for i in $(seq 1 60); do
		if curl -sf "$metrics_url" | awk '/^sandbox_runners_registered({[^}]*})?[[:space:]]+[1-9][0-9]*(\.[0-9]+)?([[:space:]]|$)/ { found=1 } END { exit(found ? 0 : 1) }'; then
			echo "Runner is registered with API."
			return 0
		fi
		if [[ -f "$API_LOG" ]] && grep -q '"msg":"runner registered"' "$API_LOG"; then
			echo "Runner is registered with API."
			return 0
		fi
		sleep 1
	done
	echo "Runner did not register with API within 60s" >&2
	return 1
}

if [[ "${E2E_SKIP_BUILD:-}" != "1" ]]; then
	echo "Building API and runner binaries..."
	make -C "$PROJECT_DIR" api runner
fi

echo "Bootstrapping e2e mTLS material..."
bash "$PROJECT_DIR/scripts/bootstrap-mtls.sh" \
	--out-dir "$TLS_DIR" \
	--api-san "$API_TLS_DNS" \
	--control-sans "localhost" \
	--force

mkdir -p "$API_DATA_DIR"
sudo mkdir -p "$RUNNER_DATA_DIR"
rm -f "$API_LOG" "$RUNNER_LOG"

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

echo "Starting Firecracker runner host process..."
sudo env \
	PATH="/usr/local/go/bin:$PATH" \
	SANDBOX_RUNNER_BACKEND=firecracker \
	SANDBOX_RUNNER_LISTEN_ADDR="$RUNNER_ADDR" \
	SANDBOX_RUNNER_DATA_DIR="$RUNNER_DATA_DIR" \
	SANDBOX_RUNNER_API_KEYS="$RUNNER_INTERNAL_API_KEY" \
	SANDBOX_RUNNER_METRICS_ENABLED=true \
	SANDBOX_RUNNER_API_GRPC_ADDR="$API_GRPC_ADDR" \
	SANDBOX_RUNNER_REGISTRATION_TOKEN="$REG_TOKEN" \
	SANDBOX_RUNNER_REGISTRATION_GRPC_CA_FILE="$TLS_DIR/runner/ca.crt" \
	SANDBOX_RUNNER_REGISTRATION_GRPC_CERT_FILE="$TLS_DIR/runner/grpc-client.crt" \
	SANDBOX_RUNNER_REGISTRATION_GRPC_KEY_FILE="$TLS_DIR/runner/grpc-client.key" \
	SANDBOX_RUNNER_REGISTRATION_GRPC_SERVER_NAME="$API_TLS_DNS" \
	SANDBOX_RUNNER_HTTP_BASE_URL="http://${RUNNER_ADDR}" \
	SANDBOX_RUNNER_CONTROL_GRPC_LISTEN_ADDR="$RUNNER_CONTROL_LISTEN_ADDR" \
	SANDBOX_RUNNER_CONTROL_GRPC_ADVERTISE_ADDR="$RUNNER_CONTROL_ADVERTISE_ADDR" \
	SANDBOX_RUNNER_CONTROL_GRPC_TLS_CERT_FILE="$TLS_DIR/runner/control-grpc-server.crt" \
	SANDBOX_RUNNER_CONTROL_GRPC_TLS_KEY_FILE="$TLS_DIR/runner/control-grpc-server.key" \
	SANDBOX_RUNNER_CONTROL_GRPC_TLS_CLIENT_CA_FILE="$TLS_DIR/runner/ca.crt" \
	SANDBOX_RUNNER_ID="e2e-firecracker-runner-$$" \
	SANDBOX_RUNNER_CAPACITY_TOTAL="${SANDBOX_RUNNER_CAPACITY_TOTAL:-4}" \
	SANDBOX_RUNNER_FIRECRACKER_PROXY_PORT_START="$FIRECRACKER_PROXY_PORT_START" \
	"$PROJECT_DIR/bin/runner" >"$RUNNER_LOG" 2>&1 &
RUNNER_PID=$!

wait_for_http "Firecracker runner" "http://${RUNNER_ADDR}/readyz"
wait_for_runner_registered

if [[ "${E2E_SKIP_BUILD:-}" != "1" ]]; then
	echo "Building SDK..."
	pnpm --dir "$PROJECT_DIR/sdk" install
	pnpm --dir "$PROJECT_DIR/sdk" build
fi

cd "$SCRIPT_DIR"
if [[ ! -d node_modules ]] || [[ ! -f node_modules/@n8n/sandbox-client/dist/index.js ]]; then
	echo "Installing e2e dependencies..."
	pnpm --dir "$SCRIPT_DIR" install --frozen-lockfile
fi

echo "Running Firecracker-compatible e2e tests..."
BASE_URL="http://127.0.0.1:$PORT" SANDBOX_API_KEY="$API_KEY" \
	npx playwright test --grep @firecracker-runner "$@"
