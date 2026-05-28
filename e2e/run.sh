#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
# shellcheck source=e2e/lib/common.sh
source "$SCRIPT_DIR/lib/common.sh"

ARCH="$(e2e_docker_arch)"
API_IMAGE="n8n-sandbox-service-api:latest-${ARCH}"
RUNNER_IMAGE="n8n-sandbox-service-runner-dind:latest-${ARCH}"
SANDBOX_IMAGE="n8n-sandbox:latest-${ARCH}"
REGISTRY_NAME="${N8N_SANDBOX_REGISTRY_NAME:-n8n-sandbox-registry}"
REGISTRY_PORT="${REGISTRY_PORT:-5050}"
REGISTRY_INTERNAL_ADDR="${REGISTRY_NAME}:5000"
PUSH_SANDBOX_IMAGE="localhost:${REGISTRY_PORT}/n8n-sandbox:e2e-${ARCH}"
REMOTE_SANDBOX_IMAGE="${REGISTRY_INTERNAL_ADDR}/n8n-sandbox:e2e-${ARCH}"
RUNNER_CONTAINER_NAME="sandbox-runner-e2e-$$"
API_CONTAINER_NAME="sandbox-api-e2e-$$"
NETWORK_NAME="sandbox-e2e-net-$$"
PORT="${PORT:-8080}"
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
  local exit_code=$?
  if [ $exit_code -ne 0 ]; then
    echo "=== API container logs ==="
    docker logs "$API_CONTAINER_NAME" 2>&1 || true
    echo "=== Runner container logs ==="
    docker logs "$RUNNER_CONTAINER_NAME" 2>&1 || true
  fi

	echo "Stopping e2e resources..."
	docker stop "$API_CONTAINER_NAME" >/dev/null 2>&1 || true
	docker rm "$API_CONTAINER_NAME" >/dev/null 2>&1 || true
	docker stop "$RUNNER_CONTAINER_NAME" >/dev/null 2>&1 || true
	docker rm "$RUNNER_CONTAINER_NAME" >/dev/null 2>&1 || true

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

e2e_bootstrap_mtls_maybe "$PROJECT_DIR" "$TLS_DIR_OWNED" "$TLS_DIR" "$API_TLS_DNS" "$RUNNER_CONTROL_ALIAS"
API_DOCKER_USER=()
API_DATA_VOLUME_ARGS=()
e2e_setup_api_container "$TLS_DIR" "$API_IMAGE"

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

API_IDLE_ENV=()
if [[ "${E2E_IDLE_TTL_SUITE:-}" == "1" ]]; then
	API_IDLE_ENV=(
		-e "SANDBOX_API_IDLE_STOP_AFTER=3s"
		-e "SANDBOX_API_IDLE_DELETE_AFTER=10s"
		-e "SANDBOX_API_IDLE_DELETE_SAFETY_BUFFER=2s"
		-e "SANDBOX_API_IDLE_SWEEP_INTERVAL=1s"
	)
fi

echo "Starting API service on port $PORT..."
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
	-v "$TLS_DIR/api:/grpc-tls:ro"
	-e "SANDBOX_API_KEYS=$API_KEY"
	-e "SANDBOX_API_METRICS_ENABLED=true"
	-e "SANDBOX_API_RUNNER_REGISTRATION_TOKEN=$REG_TOKEN"
	-e "SANDBOX_API_RUNNER_API_KEY=$RUNNER_INTERNAL_API_KEY"
	-e "SANDBOX_API_GRPC_TLS_CERT_FILE=/grpc-tls/grpc-server.crt"
	-e "SANDBOX_API_GRPC_TLS_KEY_FILE=/grpc-tls/grpc-server.key"
	-e "SANDBOX_API_GRPC_TLS_CLIENT_CA_FILE=/grpc-tls/ca.crt"
	-e "SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CA_FILE=/grpc-tls/ca.crt"
	-e "SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CERT_FILE=/grpc-tls/control-grpc-api-client.crt"
	-e "SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_KEY_FILE=/grpc-tls/control-grpc-api-client.key"
)
if ((${#API_IDLE_ENV[@]})); then
	API_DOCKER_RUN+=("${API_IDLE_ENV[@]}")
fi
API_DOCKER_RUN+=(--name "$API_CONTAINER_NAME" "$API_IMAGE")
docker run "${API_DOCKER_RUN[@]}"

e2e_wait_for_api_http "$PORT" "$API_CONTAINER_NAME"

echo "Starting runner service..."
docker run -d \
	"${RUNTIME_ARGS[@]}" \
	--network "$NETWORK_NAME" \
	--network-alias "$RUNNER_CONTROL_ALIAS" \
	-v "$TLS_DIR/runner:/grpc-tls:ro" \
	-e "SANDBOX_RUNNER_API_KEYS=$RUNNER_INTERNAL_API_KEY" \
	-e "SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE=$REMOTE_SANDBOX_IMAGE" \
	-e "SANDBOX_RUNNER_DOCKER_INSECURE_REGISTRIES=$REGISTRY_INTERNAL_ADDR" \
	-e "SANDBOX_RUNNER_API_GRPC_ADDR=${API_CONTAINER_NAME}:9090" \
	-e "SANDBOX_RUNNER_REGISTRATION_TOKEN=$REG_TOKEN" \
	-e "SANDBOX_RUNNER_REGISTRATION_GRPC_CA_FILE=/grpc-tls/ca.crt" \
	-e "SANDBOX_RUNNER_REGISTRATION_GRPC_CERT_FILE=/grpc-tls/grpc-client.crt" \
	-e "SANDBOX_RUNNER_REGISTRATION_GRPC_KEY_FILE=/grpc-tls/grpc-client.key" \
	-e "SANDBOX_RUNNER_REGISTRATION_GRPC_SERVER_NAME=$API_TLS_DNS" \
	-e "SANDBOX_RUNNER_HTTP_BASE_URL=http://${RUNNER_CONTAINER_NAME}:8080" \
	-e "SANDBOX_RUNNER_CONTROL_GRPC_LISTEN_ADDR=:9091" \
	-e "SANDBOX_RUNNER_CONTROL_GRPC_ADVERTISE_ADDR=${RUNNER_CONTROL_ALIAS}:9091" \
	-e "SANDBOX_RUNNER_CONTROL_GRPC_TLS_CERT_FILE=/grpc-tls/control-grpc-server.crt" \
	-e "SANDBOX_RUNNER_CONTROL_GRPC_TLS_KEY_FILE=/grpc-tls/control-grpc-server.key" \
	-e "SANDBOX_RUNNER_CONTROL_GRPC_TLS_CLIENT_CA_FILE=/grpc-tls/ca.crt" \
	-e "SANDBOX_RUNNER_ID=e2e-runner-$$" \
	-e "SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB=50" \
	-e "SANDBOX_RUNNER_DISK_QUOTA_POOL_SIZE_GB=2" \
	--name "$RUNNER_CONTAINER_NAME" \
	"$RUNNER_IMAGE"

echo "Waiting for runner service..."
for i in $(seq 1 60); do
	if docker exec "$RUNNER_CONTAINER_NAME" wget -q -O - http://localhost:8080/readyz >/dev/null 2>&1; then
		echo "Runner is ready."
		break
	fi
	if [[ "$i" -eq 60 ]]; then
		echo "Runner failed to start within 60s"
		docker logs "$RUNNER_CONTAINER_NAME"
		exit 1
	fi
	sleep 1
done

sleep 3

e2e_build_sdk_unless_skip "$PROJECT_DIR"
e2e_install_playwright_deps_if_needed "$SCRIPT_DIR"

export E2E_API_CONTAINER_NAME="$API_CONTAINER_NAME"
export E2E_RUNNER_CONTAINER_NAME="$RUNNER_CONTAINER_NAME"

echo "Running e2e tests (excluding topology-only + resilience; API restart runs last)..."
MAIN_SPECS=()
if [[ "${E2E_IDLE_TTL_SUITE:-}" == "1" ]]; then
	MAIN_SPECS=("tests/sandbox-idle-ttl.spec.ts")
else
	shopt -s nullglob
	for f in tests/*.spec.ts; do
		bn=$(basename "$f")
		[[ "$bn" == resilience.spec.ts ]] && continue
		[[ "$bn" == placement-no-runners.spec.ts ]] && continue
		[[ "$bn" == placement-two-runners.spec.ts ]] && continue
		[[ "$bn" == sandbox-idle-ttl.spec.ts ]] && continue
		MAIN_SPECS+=("$f")
	done
	shopt -u nullglob
fi
if [[ ${#MAIN_SPECS[@]} -eq 0 ]]; then
	echo "No Playwright specs found under tests/ (after excluding placement + resilience specs)" >&2
	exit 1
fi
BASE_URL="http://localhost:$PORT" SANDBOX_API_KEY="$API_KEY" npx playwright test "${MAIN_SPECS[@]}" "$@"

if [[ "${E2E_IDLE_TTL_SUITE:-}" != "1" ]]; then
	echo "Running API resilience e2e..."
	BASE_URL="http://localhost:$PORT" SANDBOX_API_KEY="$API_KEY" \
		npx playwright test tests/resilience.spec.ts --grep '@e2e-api-restart' "$@"
fi
