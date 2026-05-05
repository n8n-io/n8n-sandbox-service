#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

ARCH=$(uname -m | sed 's/aarch64/arm64/' | sed 's/x86_64/amd64/')
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
}
trap cleanup EXIT

if [[ "${E2E_SKIP_BUILD:-}" != "1" ]]; then
  echo "Building service and sandbox images..."
  make -C "$PROJECT_DIR" docker-local
fi

docker network create "$NETWORK_NAME" >/dev/null

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
docker run -d \
  --network "$NETWORK_NAME" \
  -p "$PORT:8080" \
  -e "SANDBOX_API_KEYS=$API_KEY" \
  -e "SANDBOX_API_RUNNER_REGISTRATION_TOKEN=$REG_TOKEN" \
  -e "SANDBOX_API_RUNNER_API_KEY=$RUNNER_INTERNAL_API_KEY" \
  --name "$API_CONTAINER_NAME" \
  "$API_IMAGE"

echo "Waiting for API service..."
for i in $(seq 1 60); do
  if curl -sf "http://localhost:$PORT/healthz" >/dev/null 2>&1; then
    echo "API is ready."
    break
  fi
  if [ "$i" -eq 60 ]; then
    echo "API failed to start within 60s"
    docker logs "$API_CONTAINER_NAME"
    exit 1
  fi
  sleep 1
done

echo "Starting runner 1..."
docker run -d \
  "${RUNTIME_ARGS[@]}" \
  --network "$NETWORK_NAME" \
  -e "SANDBOX_RUNNER_API_KEYS=$RUNNER_INTERNAL_API_KEY" \
  -e "SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE=$REMOTE_SANDBOX_IMAGE" \
  -e "SANDBOX_RUNNER_DOCKER_INSECURE_REGISTRIES=$REGISTRY_INTERNAL_ADDR" \
  -e "SANDBOX_RUNNER_API_GRPC_ADDR=${API_CONTAINER_NAME}:9090" \
  -e "SANDBOX_RUNNER_REGISTRATION_TOKEN=$REG_TOKEN" \
  -e "SANDBOX_RUNNER_HTTP_BASE_URL=http://${RUNNER1_NAME}:8080" \
  -e "SANDBOX_RUNNER_ID=e2e-runner-a-$$" \
  --name "$RUNNER1_NAME" \
  "$RUNNER_IMAGE"

echo "Starting runner 2..."
docker run -d \
  "${RUNTIME_ARGS[@]}" \
  --network "$NETWORK_NAME" \
  -e "SANDBOX_RUNNER_API_KEYS=$RUNNER_INTERNAL_API_KEY" \
  -e "SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE=$REMOTE_SANDBOX_IMAGE" \
  -e "SANDBOX_RUNNER_DOCKER_INSECURE_REGISTRIES=$REGISTRY_INTERNAL_ADDR" \
  -e "SANDBOX_RUNNER_API_GRPC_ADDR=${API_CONTAINER_NAME}:9090" \
  -e "SANDBOX_RUNNER_REGISTRATION_TOKEN=$REG_TOKEN" \
  -e "SANDBOX_RUNNER_HTTP_BASE_URL=http://${RUNNER2_NAME}:8080" \
  -e "SANDBOX_RUNNER_ID=e2e-runner-b-$$" \
  --name "$RUNNER2_NAME" \
  "$RUNNER_IMAGE"

wait_runner() {
  local name=$1
  echo "Waiting for $name..."
  for i in $(seq 1 60); do
    if docker exec "$name" wget -q -O - --header="X-Api-Key: $RUNNER_INTERNAL_API_KEY" http://localhost:8080/healthz >/dev/null 2>&1; then
      echo "$name is ready."
      return 0
    fi
    if [ "$i" -eq 60 ]; then
      echo "$name failed to start within 60s"
      docker logs "$name"
      exit 1
    fi
    sleep 1
  done
}

wait_runner "$RUNNER1_NAME"
wait_runner "$RUNNER2_NAME"

sleep 3

if [[ "${E2E_SKIP_BUILD:-}" != "1" ]]; then
  echo "Building SDK..."
  make -C "$PROJECT_DIR" sdk-install sdk-build
fi

cd "$SCRIPT_DIR"
if [ ! -d node_modules ] || [ ! -f node_modules/@n8n/sandbox-client/dist/index.js ]; then
  echo "Installing dependencies..."
  npm ci
fi

export E2E_API_CONTAINER_NAME="$API_CONTAINER_NAME"
export E2E_RUNNER1_CONTAINER_NAME="$RUNNER1_NAME"
export E2E_RUNNER2_CONTAINER_NAME="$RUNNER2_NAME"
export E2E_RUNNER_INTERNAL_API_KEY="$RUNNER_INTERNAL_API_KEY"

echo "Running two-runner placement test..."
BASE_URL="http://localhost:$PORT" SANDBOX_API_KEY="$API_KEY" npx playwright test tests/placement-two-runners.spec.ts "$@"

echo "Running runner failure resilience e2e..."
BASE_URL="http://localhost:$PORT" SANDBOX_API_KEY="$API_KEY" \
  npx playwright test tests/resilience.spec.ts -g "stopped runner" "$@"
