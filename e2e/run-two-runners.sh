#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
# shellcheck source=e2e/lib/common.sh
source "$SCRIPT_DIR/lib/common.sh"

ARCH="$(e2e_docker_arch)"
API_IMAGE="n8n-sandbox-api:latest-${ARCH}"
RUNNER_IMAGE="n8n-sandbox-runner:latest-${ARCH}"
SANDBOX_IMAGE="n8n-sandbox:latest-${ARCH}"
REGISTRY_NAME="${N8N_SANDBOX_REGISTRY_NAME:-n8n-sandbox-registry}"
REGISTRY_PORT="${REGISTRY_PORT:-5050}"
REGISTRY_INTERNAL_ADDR="${REGISTRY_NAME}:5000"
PUSH_SANDBOX_IMAGE="localhost:${REGISTRY_PORT}/n8n-sandbox:e2e-${ARCH}"
REMOTE_SANDBOX_IMAGE="${REGISTRY_INTERNAL_ADDR}/n8n-sandbox:e2e-${ARCH}"
RUNNER1_NAME="sandbox-runner-e2e-a-$$"
RUNNER2_NAME="sandbox-runner-e2e-b-$$"
API_CONTAINER_NAME="sandbox-api-e2e-2r-$$"
NETWORK_NAME="sandbox-e2e-net-2r-$$"
PORT="${PORT:-18081}"
API_KEY="test"
RUNNER_INTERNAL_API_KEY="runner-test"
REG_TOKEN="${SANDBOX_API_RUNNER_REGISTRATION_TOKEN:-e2e-reg-token}"
STARTED_REGISTRY=false
TLS_DIR="${E2E_TLS_DIR:-$(mktemp -d)}"
TLS_DIR_OWNED=0
if [[ -z "${E2E_TLS_DIR:-}" ]]; then
	TLS_DIR_OWNED=1
fi
API_TLS_DNS="${E2E_API_TLS_DNS:-sandbox-api-e2e-mtls}"
RUNNER1_CONTROL_ALIAS="runner-control-a"
RUNNER2_CONTROL_ALIAS="runner-control-b"

cleanup() {
	echo "Stopping e2e resources..."
	docker stop "$API_CONTAINER_NAME" >/dev/null 2>&1 || true
	docker rm "$API_CONTAINER_NAME" >/dev/null 2>&1 || true
	docker stop "$RUNNER1_NAME" >/dev/null 2>&1 || true
	docker rm "$RUNNER1_NAME" >/dev/null 2>&1 || true
	docker stop "$RUNNER2_NAME" >/dev/null 2>&1 || true
	docker rm "$RUNNER2_NAME" >/dev/null 2>&1 || true

	if ! $STARTED_REGISTRY && docker network inspect "$NETWORK_NAME" >/dev/null 2>&1; then
		docker network disconnect "$NETWORK_NAME" "$REGISTRY_NAME" >/dev/null 2>&1 || true
	fi

	docker network rm "$NETWORK_NAME" >/dev/null 2>&1 || true
	if $STARTED_REGISTRY; then
		docker stop "$REGISTRY_NAME" >/dev/null 2>&1 || true
		docker rm "$REGISTRY_NAME" >/dev/null 2>&1 || true
	fi
	if [[ "$TLS_DIR_OWNED" == "1" ]]; then
		rm -rf "$TLS_DIR"
	fi
}
trap cleanup EXIT

if [[ "${E2E_SKIP_BUILD:-}" != "1" ]]; then
	echo "Building service and sandbox images..."
	make -C "$PROJECT_DIR" docker-local
fi

e2e_bootstrap_mtls_maybe "$PROJECT_DIR" "$TLS_DIR_OWNED" "$TLS_DIR" "$API_TLS_DNS" "${RUNNER1_CONTROL_ALIAS},${RUNNER2_CONTROL_ALIAS}"
e2e_normalize_tls_permissions "$TLS_DIR"
API_DOCKER_USER=()
e2e_setup_api_tls_for_container "$TLS_DIR" "$API_IMAGE"

e2e_docker_network_create "$NETWORK_NAME"

if ! docker ps --format '{{.Names}}' | grep -qx "${REGISTRY_NAME}"; then
	echo "Starting local registry..."
	docker rm -f "${REGISTRY_NAME}" >/dev/null 2>&1 || true
	docker run -d \
		--restart unless-stopped \
		--name "$REGISTRY_NAME" \
		--network "$NETWORK_NAME" \
		-p "${REGISTRY_PORT}:5000" \
		registry:2 >/dev/null
	STARTED_REGISTRY=true
else
	docker network connect "$NETWORK_NAME" "$REGISTRY_NAME" >/dev/null 2>&1 || true
fi

if [[ "${STARTED_REGISTRY:-}" == "true" ]]; then
	e2e_wait_for_registry "$REGISTRY_PORT"
fi

echo "Pushing sandbox image to local registry..."
docker tag "$SANDBOX_IMAGE" "$PUSH_SANDBOX_IMAGE"
docker push "$PUSH_SANDBOX_IMAGE"

RUNTIME_ARGS=()
if [[ "${PRIVILEGED:-}" == "1" ]] || [[ "$(uname)" == "Darwin" ]]; then
	RUNTIME_ARGS+=(--privileged)
else
	RUNTIME_ARGS+=(--runtime=sysbox-runc)
fi

echo "Starting API service on port $PORT..."
API_DOCKER_RUN=(-d)
if ((${#API_DOCKER_USER[@]})); then
	API_DOCKER_RUN+=("${API_DOCKER_USER[@]}")
fi
API_DOCKER_RUN+=(
	--network "$NETWORK_NAME"
	-p "$PORT:8080"
	-v "$TLS_DIR:/grpc-tls:ro"
	-e "SANDBOX_API_KEYS=$API_KEY"
	-e "SANDBOX_API_RUNNER_REGISTRATION_TOKEN=$REG_TOKEN"
	-e "SANDBOX_API_RUNNER_API_KEY=$RUNNER_INTERNAL_API_KEY"
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

echo "Starting runner 1..."
docker run -d \
	"${RUNTIME_ARGS[@]}" \
	--network "$NETWORK_NAME" \
	--network-alias "$RUNNER1_CONTROL_ALIAS" \
	-v "$TLS_DIR:/grpc-tls:ro" \
	-e "SANDBOX_RUNNER_API_KEYS=$RUNNER_INTERNAL_API_KEY" \
	-e "SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE=$REMOTE_SANDBOX_IMAGE" \
	-e "SANDBOX_RUNNER_DOCKER_INSECURE_REGISTRIES=$REGISTRY_INTERNAL_ADDR" \
	-e "SANDBOX_RUNNER_API_GRPC_ADDR=${API_CONTAINER_NAME}:9090" \
	-e "SANDBOX_RUNNER_REGISTRATION_TOKEN=$REG_TOKEN" \
	-e "SANDBOX_RUNNER_REGISTRATION_GRPC_CA_FILE=/grpc-tls/ca.crt" \
	-e "SANDBOX_RUNNER_REGISTRATION_GRPC_CERT_FILE=/grpc-tls/grpc-client.crt" \
	-e "SANDBOX_RUNNER_REGISTRATION_GRPC_KEY_FILE=/grpc-tls/grpc-client.key" \
	-e "SANDBOX_RUNNER_REGISTRATION_GRPC_SERVER_NAME=$API_TLS_DNS" \
	-e "SANDBOX_RUNNER_HTTP_BASE_URL=http://${RUNNER1_NAME}:8080" \
	-e "SANDBOX_RUNNER_CONTROL_GRPC_LISTEN_ADDR=:9091" \
	-e "SANDBOX_RUNNER_CONTROL_GRPC_ADVERTISE_ADDR=${RUNNER1_CONTROL_ALIAS}:9091" \
	-e "SANDBOX_RUNNER_CONTROL_GRPC_TLS_CERT_FILE=/grpc-tls/control-grpc-server.crt" \
	-e "SANDBOX_RUNNER_CONTROL_GRPC_TLS_KEY_FILE=/grpc-tls/control-grpc-server.key" \
	-e "SANDBOX_RUNNER_CONTROL_GRPC_TLS_CLIENT_CA_FILE=/grpc-tls/ca.crt" \
	-e "SANDBOX_RUNNER_ID=e2e-runner-a-$$" \
	--name "$RUNNER1_NAME" \
	"$RUNNER_IMAGE"

echo "Starting runner 2..."
docker run -d \
	"${RUNTIME_ARGS[@]}" \
	--network "$NETWORK_NAME" \
	--network-alias "$RUNNER2_CONTROL_ALIAS" \
	-v "$TLS_DIR:/grpc-tls:ro" \
	-e "SANDBOX_RUNNER_API_KEYS=$RUNNER_INTERNAL_API_KEY" \
	-e "SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE=$REMOTE_SANDBOX_IMAGE" \
	-e "SANDBOX_RUNNER_DOCKER_INSECURE_REGISTRIES=$REGISTRY_INTERNAL_ADDR" \
	-e "SANDBOX_RUNNER_API_GRPC_ADDR=${API_CONTAINER_NAME}:9090" \
	-e "SANDBOX_RUNNER_REGISTRATION_TOKEN=$REG_TOKEN" \
	-e "SANDBOX_RUNNER_REGISTRATION_GRPC_CA_FILE=/grpc-tls/ca.crt" \
	-e "SANDBOX_RUNNER_REGISTRATION_GRPC_CERT_FILE=/grpc-tls/grpc-client.crt" \
	-e "SANDBOX_RUNNER_REGISTRATION_GRPC_KEY_FILE=/grpc-tls/grpc-client.key" \
	-e "SANDBOX_RUNNER_REGISTRATION_GRPC_SERVER_NAME=$API_TLS_DNS" \
	-e "SANDBOX_RUNNER_HTTP_BASE_URL=http://${RUNNER2_NAME}:8080" \
	-e "SANDBOX_RUNNER_CONTROL_GRPC_LISTEN_ADDR=:9091" \
	-e "SANDBOX_RUNNER_CONTROL_GRPC_ADVERTISE_ADDR=${RUNNER2_CONTROL_ALIAS}:9091" \
	-e "SANDBOX_RUNNER_CONTROL_GRPC_TLS_CERT_FILE=/grpc-tls/control-grpc-server.crt" \
	-e "SANDBOX_RUNNER_CONTROL_GRPC_TLS_KEY_FILE=/grpc-tls/control-grpc-server.key" \
	-e "SANDBOX_RUNNER_CONTROL_GRPC_TLS_CLIENT_CA_FILE=/grpc-tls/ca.crt" \
	-e "SANDBOX_RUNNER_ID=e2e-runner-b-$$" \
	--name "$RUNNER2_NAME" \
	"$RUNNER_IMAGE"

wait_runner() {
	local name=$1
	local i
	echo "Waiting for $name..."
	for i in $(seq 1 60); do
		if docker exec "$name" wget -q -O - --header="X-Api-Key: $RUNNER_INTERNAL_API_KEY" http://localhost:8080/healthz >/dev/null 2>&1; then
			echo "$name is ready."
			return 0
		fi
		if [[ "$i" -eq 60 ]]; then
			echo "$name failed to start within 60s"
			docker logs "$name"
			exit 1
		fi
		sleep 1
	done
}

wait_runner "$RUNNER1_NAME"
wait_runner "$RUNNER2_NAME"

sleep 6

e2e_build_sdk_unless_skip "$PROJECT_DIR"
e2e_install_playwright_deps_if_needed "$SCRIPT_DIR"

export E2E_API_CONTAINER_NAME="$API_CONTAINER_NAME"
export E2E_RUNNER1_CONTAINER_NAME="$RUNNER1_NAME"
export E2E_RUNNER2_CONTAINER_NAME="$RUNNER2_NAME"
export E2E_RUNNER_INTERNAL_API_KEY="$RUNNER_INTERNAL_API_KEY"

echo "Running two-runner placement test..."
BASE_URL="http://localhost:$PORT" SANDBOX_API_KEY="$API_KEY" npx playwright test tests/placement-two-runners.spec.ts "$@"

echo "Running runner failure resilience e2e..."
BASE_URL="http://localhost:$PORT" SANDBOX_API_KEY="$API_KEY" \
	npx playwright test tests/resilience.spec.ts --grep '@e2e-stopped-runner' "$@"
