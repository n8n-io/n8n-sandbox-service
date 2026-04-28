#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

ARCH=$(uname -m | sed 's/aarch64/arm64/' | sed 's/x86_64/amd64/')
SERVICE_IMAGE="n8n-sandbox-service:latest-${ARCH}"
SANDBOX_IMAGE="n8n-sandbox:latest-${ARCH}"
REGISTRY_NAME="${N8N_SANDBOX_REGISTRY_NAME:-n8n-sandbox-registry}"
REGISTRY_PORT="${REGISTRY_PORT:-5050}"
REGISTRY_ADDR="host.docker.internal:${REGISTRY_PORT}"
PUSH_SANDBOX_IMAGE="localhost:${REGISTRY_PORT}/n8n-sandbox:e2e-${ARCH}"
REMOTE_SANDBOX_IMAGE="${REGISTRY_ADDR}/n8n-sandbox:e2e-${ARCH}"
CONTAINER_NAME="sandbox-e2e-$$"
PORT="${PORT:-8080}"
API_KEY="test"
STARTED_REGISTRY=false

cleanup() {
  echo "Stopping e2e resources..."
  docker stop "$CONTAINER_NAME" >/dev/null 2>&1 || true
  docker rm "$CONTAINER_NAME" >/dev/null 2>&1 || true
  if $STARTED_REGISTRY; then
    docker stop "$REGISTRY_NAME" >/dev/null 2>&1 || true
    docker rm "$REGISTRY_NAME" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

echo "Building service and sandbox images..."
make -C "$PROJECT_DIR" docker-local

if ! docker ps --format '{{.Names}}' | grep -qx "${REGISTRY_NAME}"; then
  echo "Starting local registry..."
  docker rm -f "${REGISTRY_NAME}" >/dev/null 2>&1 || true
  docker run -d --restart unless-stopped --name "$REGISTRY_NAME" -p "${REGISTRY_PORT}:5000" registry:2 >/dev/null
  STARTED_REGISTRY=true
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

echo "Starting sandbox service on port $PORT..."
docker run -d \
  "${RUNTIME_ARGS[@]}" \
  --add-host host.docker.internal:host-gateway \
  -p "$PORT:8080" \
  -e "SANDBOX_API_KEYS=$API_KEY" \
  -e "SANDBOX_DOCKER_SANDBOX_IMAGE=$REMOTE_SANDBOX_IMAGE" \
  -e "SANDBOX_DOCKER_INSECURE_REGISTRIES=$REGISTRY_ADDR" \
  --name "$CONTAINER_NAME" \
  "$SERVICE_IMAGE"

echo "Waiting for service..."
for i in $(seq 1 60); do
  if curl -sf "http://localhost:$PORT/healthz" >/dev/null 2>&1; then
    echo "Service is ready."
    break
  fi
  if [ "$i" -eq 60 ]; then
    echo "Service failed to start within 60s"
    docker logs "$CONTAINER_NAME"
    exit 1
  fi
  sleep 1
done

cd "$SCRIPT_DIR"
if [ ! -d node_modules ]; then
  echo "Installing dependencies..."
  npm install
fi

echo "Running e2e tests..."
BASE_URL="http://localhost:$PORT" SANDBOX_API_KEY="$API_KEY" npx playwright test "$@"
