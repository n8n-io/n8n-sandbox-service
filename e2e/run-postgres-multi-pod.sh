#!/usr/bin/env bash
# Two API pods + Postgres + one Docker runner. Runner registers on pod A; tests use pod B.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
# shellcheck source=e2e/lib/common.sh
source "$SCRIPT_DIR/lib/common.sh"

export E2E_STORE=postgres

ARCH="$(e2e_docker_arch)"
API_IMAGE="n8n-sandbox-service-api:latest-${ARCH}"
RUNNER_IMAGE="n8n-sandbox-service-runner-dind:latest-${ARCH}"
SANDBOX_IMAGE="n8n-sandbox:latest-${ARCH}"
REGISTRY_NAME="${N8N_SANDBOX_REGISTRY_NAME:-n8n-sandbox-registry}"
REGISTRY_PORT="${REGISTRY_PORT:-5050}"
REGISTRY_INTERNAL_ADDR="${REGISTRY_NAME}:5000"
PUSH_SANDBOX_IMAGE="localhost:${REGISTRY_PORT}/n8n-sandbox:e2e-${ARCH}"
REMOTE_SANDBOX_IMAGE="${REGISTRY_INTERNAL_ADDR}/n8n-sandbox:e2e-${ARCH}"
API1_NAME="sandbox-api-e2e-mp-a-$$"
API2_NAME="sandbox-api-e2e-mp-b-$$"
POSTGRES_CONTAINER_NAME="sandbox-postgres-e2e-mp-$$"
RUNNER_NAME="sandbox-runner-e2e-mp-$$"
NETWORK_NAME="sandbox-e2e-net-mp-$$"
PORT_A="${PORT_A:-18092}"
PORT_B="${PORT_B:-18093}"
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
RUNNER_CONTROL_ALIAS="runner-control"

cleanup() {
	echo "Stopping multi-pod e2e resources..."
	docker stop "$API1_NAME" "$API2_NAME" "$RUNNER_NAME" >/dev/null 2>&1 || true
	docker rm "$API1_NAME" "$API2_NAME" "$RUNNER_NAME" >/dev/null 2>&1 || true
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
	e2e_stop_postgres_container "$POSTGRES_CONTAINER_NAME"
}
trap cleanup EXIT

if [[ "${E2E_SKIP_BUILD:-}" != "1" ]]; then
	echo "Building service and sandbox images..."
	make -C "$PROJECT_DIR" docker-local
fi

e2e_bootstrap_mtls_maybe "$PROJECT_DIR" "$TLS_DIR_OWNED" "$TLS_DIR" "$API_TLS_DNS" "$RUNNER_CONTROL_ALIAS"
API_DOCKER_USER=()
API_DATA_VOLUME_ARGS=()
e2e_setup_api_container "$TLS_DIR" "$API_IMAGE"

e2e_docker_network_create "$NETWORK_NAME"

API_STORE_ENV=()
e2e_configure_api_store "$NETWORK_NAME" "$POSTGRES_CONTAINER_NAME"

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

API_IDLE_ENV=()
start_api_pod() {
	local name=$1 port=$2
	local -a run=(-d)
	if ((${#API_DOCKER_USER[@]})); then
		run+=("${API_DOCKER_USER[@]}")
	fi
	run+=(
		--network "$NETWORK_NAME"
		-p "${port}:8080"
	)
	e2e_api_container_env_args "$API_KEY" "$REG_TOKEN" "$RUNNER_INTERNAL_API_KEY" "$TLS_DIR"
	run+=("${E2E_API_CONTAINER_ENV_ARGS[@]}")
	run+=(--name "$name" "$API_IMAGE")
	docker run "${run[@]}"
	e2e_wait_for_api_http "$port" "$name"
}

echo "Starting API pod A on port $PORT_A..."
start_api_pod "$API1_NAME" "$PORT_A"

echo "Starting API pod B on port $PORT_B..."
start_api_pod "$API2_NAME" "$PORT_B"

RUNTIME_ARGS=()
if [[ "${PRIVILEGED:-}" == "1" ]] || [[ "$(uname)" == "Darwin" ]]; then
	RUNTIME_ARGS+=(--privileged)
else
	RUNTIME_ARGS+=(--runtime=sysbox-runc)
fi

echo "Starting runner (registers on API pod A)..."
docker run -d \
	"${RUNTIME_ARGS[@]}" \
	--network "$NETWORK_NAME" \
	--network-alias "$RUNNER_CONTROL_ALIAS" \
	-v "$TLS_DIR/runner:/grpc-tls:ro" \
	-e "SANDBOX_RUNNER_API_KEYS=$RUNNER_INTERNAL_API_KEY" \
	-e "SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE=$REMOTE_SANDBOX_IMAGE" \
	-e "SANDBOX_RUNNER_DOCKER_INSECURE_REGISTRIES=$REGISTRY_INTERNAL_ADDR" \
	-e "SANDBOX_RUNNER_API_GRPC_ADDR=${API1_NAME}:9090" \
	-e "SANDBOX_RUNNER_REGISTRATION_TOKEN=$REG_TOKEN" \
	-e "SANDBOX_RUNNER_REGISTRATION_GRPC_CA_FILE=/grpc-tls/ca.crt" \
	-e "SANDBOX_RUNNER_REGISTRATION_GRPC_CERT_FILE=/grpc-tls/grpc-client.crt" \
	-e "SANDBOX_RUNNER_REGISTRATION_GRPC_KEY_FILE=/grpc-tls/grpc-client.key" \
	-e "SANDBOX_RUNNER_REGISTRATION_GRPC_SERVER_NAME=$API_TLS_DNS" \
	-e "SANDBOX_RUNNER_HTTP_BASE_URL=http://${RUNNER_NAME}:8080" \
	-e "SANDBOX_RUNNER_CONTROL_GRPC_LISTEN_ADDR=:9091" \
	-e "SANDBOX_RUNNER_CONTROL_GRPC_ADVERTISE_ADDR=${RUNNER_CONTROL_ALIAS}:9091" \
	-e "SANDBOX_RUNNER_CONTROL_GRPC_TLS_CERT_FILE=/grpc-tls/control-grpc-server.crt" \
	-e "SANDBOX_RUNNER_CONTROL_GRPC_TLS_KEY_FILE=/grpc-tls/control-grpc-server.key" \
	-e "SANDBOX_RUNNER_CONTROL_GRPC_TLS_CLIENT_CA_FILE=/grpc-tls/ca.crt" \
	-e "SANDBOX_RUNNER_ID=e2e-runner-mp-$$" \
	--name "$RUNNER_NAME" \
	"$RUNNER_IMAGE"

for i in $(seq 1 60); do
	if docker exec "$RUNNER_NAME" wget -q -O - http://localhost:8080/readyz >/dev/null 2>&1; then
		echo "Runner is ready."
		break
	fi
	if [[ "$i" -eq 60 ]]; then
		echo "Runner failed to start within 60s"
		docker logs "$RUNNER_NAME"
		exit 1
	fi
	sleep 1
done

sleep 5

e2e_build_sdk_unless_skip "$PROJECT_DIR"
e2e_install_playwright_deps_if_needed "$SCRIPT_DIR"

echo "Running multi-pod API tests..."
E2E_MULTI_POD=1 \
BASE_URL_A="http://localhost:$PORT_A" \
BASE_URL_B="http://localhost:$PORT_B" \
SANDBOX_API_KEY="$API_KEY" \
	npx playwright test tests/multi-pod-api.spec.ts "$@"
