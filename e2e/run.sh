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
REGISTRY_ADDR="localhost:${REGISTRY_PORT}"
REGISTRY_INTERNAL_ADDR="${REGISTRY_NAME}:5000"
PUSH_SANDBOX_IMAGE="localhost:${REGISTRY_PORT}/n8n-sandbox:e2e-${ARCH}"
REMOTE_SANDBOX_IMAGE="${REGISTRY_INTERNAL_ADDR}/n8n-sandbox:e2e-${ARCH}"
RUNNER_CONTAINER_NAME="sandbox-runner-e2e-$$"
API_CONTAINER_NAME="sandbox-api-e2e-$$"
NETWORK_NAME="sandbox-e2e-net-$$"
PORT="${PORT:-8080}"
API_KEY="test"
RUNNER_INTERNAL_API_KEY="runner-test"
REG_TOKEN="${SANDBOX_RUNNER_REGISTRATION_TOKEN:-e2e-reg-token}"
STARTED_REGISTRY=false

cleanup() {
  echo "Stopping e2e resources..."
  docker stop "$API_CONTAINER_NAME" >/dev/null 2>&1 || true
  docker rm "$API_CONTAINER_NAME" >/dev/null 2>&1 || true
  docker stop "$RUNNER_CONTAINER_NAME" >/dev/null 2>&1 || true
  docker rm "$RUNNER_CONTAINER_NAME" >/dev/null 2>&1 || true

  # If we reused an existing registry, disconnect it from our e2e network before cleanup
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
  # Reused registry may not be attached to this e2e network.
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
  -e "SANDBOX_RUNNER_REGISTRATION_TOKEN=$REG_TOKEN" \
  -e "SANDBOX_RUNNER_API_KEY=$RUNNER_INTERNAL_API_KEY" \
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

echo "Starting runner service..."
docker run -d \
  "${RUNTIME_ARGS[@]}" \
  --network "$NETWORK_NAME" \
  -e "SANDBOX_API_KEYS=$RUNNER_INTERNAL_API_KEY" \
  -e "SANDBOX_DOCKER_SANDBOX_IMAGE=$REMOTE_SANDBOX_IMAGE" \
  -e "SANDBOX_DOCKER_INSECURE_REGISTRIES=$REGISTRY_INTERNAL_ADDR" \
  -e "SANDBOX_API_GRPC_ADDR=${API_CONTAINER_NAME}:9090" \
  -e "SANDBOX_RUNNER_REGISTRATION_TOKEN=$REG_TOKEN" \
  -e "SANDBOX_RUNNER_HTTP_BASE_URL=http://${RUNNER_CONTAINER_NAME}:8080" \
  -e "SANDBOX_RUNNER_ID=e2e-runner-$$" \
  --name "$RUNNER_CONTAINER_NAME" \
  "$RUNNER_IMAGE"

echo "Waiting for runner service..."
for i in $(seq 1 60); do
  if docker exec "$RUNNER_CONTAINER_NAME" wget -q -O - --header="X-Api-Key: $RUNNER_INTERNAL_API_KEY" http://localhost:8080/healthz >/dev/null 2>&1; then
    echo "Runner is ready."
    break
  fi
  if [ "$i" -eq 60 ]; then
    echo "Runner failed to start within 60s"
    docker logs "$RUNNER_CONTAINER_NAME"
    exit 1
  fi
  sleep 1
done

# Allow gRPC registration heartbeats to reach the API before placement tests run.
sleep 3

cd "$SCRIPT_DIR"
if [ ! -d node_modules ]; then
  echo "Installing dependencies..."
  npm install
fi

export E2E_API_CONTAINER_NAME="$API_CONTAINER_NAME"
export E2E_RUNNER_CONTAINER_NAME="$RUNNER_CONTAINER_NAME"

echo "Running e2e tests (excluding topology-only + resilience; API restart runs last)..."
MAIN_SPECS=()
shopt -s nullglob
for f in tests/*.spec.ts; do
  bn=$(basename "$f")
  [[ "$bn" == resilience.spec.ts ]] && continue
  [[ "$bn" == placement-no-runners.spec.ts ]] && continue
  [[ "$bn" == placement-two-runners.spec.ts ]] && continue
  MAIN_SPECS+=("$f")
done
shopt -u nullglob
if [[ ${#MAIN_SPECS[@]} -eq 0 ]]; then
  echo "No Playwright specs found under tests/ (after excluding placement + resilience specs)" >&2
  exit 1
fi
BASE_URL="http://localhost:$PORT" SANDBOX_API_KEY="$API_KEY" npx playwright test "${MAIN_SPECS[@]}" "$@"

echo "Running API resilience e2e..."
BASE_URL="http://localhost:$PORT" SANDBOX_API_KEY="$API_KEY" \
  npx playwright test tests/resilience.spec.ts -g "API container restart" "$@"
