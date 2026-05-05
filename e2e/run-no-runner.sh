#!/usr/bin/env bash
# Starts only the API (no runner containers). Requires SANDBOX_API_RUNNER_REGISTRATION_TOKEN on the API.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

ARCH=$(uname -m | sed 's/aarch64/arm64/' | sed 's/x86_64/amd64/')
API_IMAGE="n8n-sandbox-api:latest-${ARCH}"
API_CONTAINER_NAME="sandbox-api-e2e-norunner-$$"
NETWORK_NAME="sandbox-e2e-net-norunner-$$"
PORT="${PORT:-18080}"
API_KEY="test"
REG_TOKEN="${SANDBOX_API_RUNNER_REGISTRATION_TOKEN:-e2e-reg-token}"

cleanup() {
  echo "Stopping e2e resources..."
  docker stop "$API_CONTAINER_NAME" >/dev/null 2>&1 || true
  docker rm "$API_CONTAINER_NAME" >/dev/null 2>&1 || true
  docker network rm "$NETWORK_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

if [[ "${E2E_SKIP_BUILD:-}" != "1" ]]; then
  echo "Building API image..."
  make -C "$PROJECT_DIR" "docker-api-${ARCH}"
fi

docker network create "$NETWORK_NAME" >/dev/null

echo "Starting API only on port $PORT..."
docker run -d \
  --network "$NETWORK_NAME" \
  -p "$PORT:8080" \
  -e "SANDBOX_API_KEYS=$API_KEY" \
  -e "SANDBOX_API_RUNNER_REGISTRATION_TOKEN=$REG_TOKEN" \
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

if [[ "${E2E_SKIP_BUILD:-}" != "1" ]]; then
  echo "Building SDK..."
  make -C "$PROJECT_DIR" sdk-install sdk-build
fi

cd "$SCRIPT_DIR"
if [ ! -d node_modules ]; then
  echo "Installing dependencies..."
  npm install
fi

echo "Running no-runner placement test..."
BASE_URL="http://localhost:$PORT" SANDBOX_API_KEY="$API_KEY" npx playwright test tests/placement-no-runners.spec.ts "$@"
