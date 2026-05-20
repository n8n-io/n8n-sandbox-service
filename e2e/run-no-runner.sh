#!/usr/bin/env bash
# Starts only the API (no runner containers). Requires SANDBOX_API_RUNNER_REGISTRATION_TOKEN on the API.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
# shellcheck source=e2e/lib/common.sh
source "$SCRIPT_DIR/lib/common.sh"

ARCH="$(e2e_docker_arch)"
API_IMAGE="n8n-sandbox-service-api:latest-${ARCH}"
API_CONTAINER_NAME="sandbox-api-e2e-norunner-$$"
NETWORK_NAME="sandbox-e2e-net-norunner-$$"
PORT="${PORT:-18080}"
API_KEY="test"
REG_TOKEN="${SANDBOX_API_RUNNER_REGISTRATION_TOKEN:-e2e-reg-token}"
TLS_DIR="${E2E_TLS_DIR:-$(mktemp -d)}"
TLS_DIR_OWNED=0
if [[ -z "${E2E_TLS_DIR:-}" ]]; then
	TLS_DIR_OWNED=1
fi
API_TLS_DNS="${E2E_API_TLS_DNS:-sandbox-api-e2e-mtls}"

cleanup() {
	echo "Stopping e2e resources..."
	docker stop "$API_CONTAINER_NAME" >/dev/null 2>&1 || true
	docker rm "$API_CONTAINER_NAME" >/dev/null 2>&1 || true
	docker network rm "$NETWORK_NAME" >/dev/null 2>&1 || true
	if [[ "$TLS_DIR_OWNED" == "1" ]]; then
		rm -rf "$TLS_DIR"
	fi
}
trap cleanup EXIT

if [[ "${E2E_SKIP_BUILD:-}" != "1" ]]; then
	echo "Building API image..."
	make -C "$PROJECT_DIR" "docker-api-${ARCH}"
fi

e2e_bootstrap_mtls_maybe "$PROJECT_DIR" "$TLS_DIR_OWNED" "$TLS_DIR" "$API_TLS_DNS" "runner-control"
e2e_normalize_tls_permissions "$TLS_DIR"
API_DOCKER_USER=()
API_DATA_VOLUME_ARGS=()
e2e_setup_api_tls_for_container "$TLS_DIR" "$API_IMAGE"

e2e_docker_network_create "$NETWORK_NAME"

echo "Starting API only on port $PORT..."
API_DOCKER_RUN=(-d)
if ((${#API_DOCKER_USER[@]})); then
	API_DOCKER_RUN+=("${API_DOCKER_USER[@]}")
fi
if ((${#API_DATA_VOLUME_ARGS[@]})); then
	API_DOCKER_RUN+=("${API_DATA_VOLUME_ARGS[@]}")
fi
API_DOCKER_RUN+=(
	--network "$NETWORK_NAME"
	-p "$PORT:8080"
	-v "$TLS_DIR:/grpc-tls:ro"
	-e "SANDBOX_API_KEYS=$API_KEY"
	-e "SANDBOX_API_RUNNER_REGISTRATION_TOKEN=$REG_TOKEN"
	-e "SANDBOX_API_GRPC_TLS_CERT_FILE=/grpc-tls/grpc-server.crt"
	-e "SANDBOX_API_GRPC_TLS_KEY_FILE=/grpc-tls/grpc-server.key"
	-e "SANDBOX_API_GRPC_TLS_CLIENT_CA_FILE=/grpc-tls/ca.crt"
	-e "SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CA_FILE=/grpc-tls/ca.crt"
	-e "SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CERT_FILE=/grpc-tls/control-grpc-api-client.crt"
	-e "SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_KEY_FILE=/grpc-tls/control-grpc-api-client.key"
	--name "$API_CONTAINER_NAME"
	"$API_IMAGE"
)
docker run "${API_DOCKER_RUN[@]}"

e2e_wait_for_api_http "$PORT" "$API_CONTAINER_NAME"

e2e_build_sdk_unless_skip "$PROJECT_DIR"
e2e_install_playwright_deps_if_needed "$SCRIPT_DIR"

echo "Running no-runner placement test..."
BASE_URL="http://localhost:$PORT" SANDBOX_API_KEY="$API_KEY" npx playwright test tests/placement-no-runners.spec.ts "$@"
