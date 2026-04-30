#!/usr/bin/env bash
set -euo pipefail

ARCH=$(uname -m | sed 's/aarch64/arm64/' | sed 's/x86_64/amd64/')
REGISTRY_NAME=${N8N_SANDBOX_REGISTRY_NAME:-n8n-sandbox-registry}
REGISTRY_ADDR=${N8N_SANDBOX_REGISTRY_ADDR:-host.docker.internal:5050}
API_IMAGE_LOCAL="n8n-sandbox-api:latest-${ARCH}"
RUNNER_IMAGE_LOCAL="n8n-sandbox-runner:latest-${ARCH}"
SANDBOX_IMAGE_LOCAL="n8n-sandbox:latest-${ARCH}"
REGISTRY_PORT=${REGISTRY_ADDR##*:}
SANDBOX_IMAGE_PUSH="localhost:${REGISTRY_PORT}/n8n-sandbox:latest-${ARCH}"
SANDBOX_IMAGE_REMOTE="${REGISTRY_ADDR}/n8n-sandbox:latest-${ARCH}"
API_CONTAINER_NAME=${N8N_SANDBOX_API_CONTAINER_NAME:-n8n-sandbox-api-local}
RUNNER1_CONTAINER_NAME=${N8N_SANDBOX_RUNNER1_CONTAINER_NAME:-n8n-sandbox-runner-local-1}
RUNNER2_CONTAINER_NAME=${N8N_SANDBOX_RUNNER2_CONTAINER_NAME:-n8n-sandbox-runner-local-2}
NETWORK_NAME=${N8N_SANDBOX_LOCAL_NETWORK:-n8n-sandbox-local}
RUNNER_INTERNAL_API_KEY=${N8N_SANDBOX_RUNNER_API_KEY:-runner-local-key}
REG_TOKEN=${SANDBOX_API_RUNNER_REGISTRATION_TOKEN:-local-reg-token}

echo "==> Building API, runner, and sandbox images for ${ARCH} ..."
make docker-local

if ! docker ps --format '{{.Names}}' | grep -qx "${REGISTRY_NAME}"; then
  echo "==> Starting local registry ${REGISTRY_NAME} on port 5050 ..."
  docker rm -f "${REGISTRY_NAME}" >/dev/null 2>&1 || true
  docker run -d --restart unless-stopped --name "${REGISTRY_NAME}" -p 5050:5000 registry:2 >/dev/null
fi

echo "==> Pushing sandbox image to local registry ..."
docker tag "${SANDBOX_IMAGE_LOCAL}" "${SANDBOX_IMAGE_PUSH}"
docker push "${SANDBOX_IMAGE_PUSH}"

if ! docker network inspect "${NETWORK_NAME}" >/dev/null 2>&1; then
  echo "==> Creating local network ${NETWORK_NAME} ..."
  docker network create "${NETWORK_NAME}" >/dev/null
fi

docker rm -f "${API_CONTAINER_NAME}" >/dev/null 2>&1 || true
docker rm -f "${RUNNER1_CONTAINER_NAME}" >/dev/null 2>&1 || true
docker rm -f "${RUNNER2_CONTAINER_NAME}" >/dev/null 2>&1 || true

RUNTIME_ARGS=()
if [[ "$(uname)" == "Darwin" ]]; then
  echo "==> Running runner containers with --privileged (macOS, no sysbox) ..."
  RUNTIME_ARGS+=(--privileged)
else
  echo "==> Running runner containers with sysbox ..."
  RUNTIME_ARGS+=(--runtime=sysbox-runc)
fi

echo "==> Starting API container on localhost:8080 ..."
docker run -d \
  --name "${API_CONTAINER_NAME}" \
  --network "${NETWORK_NAME}" \
  -p 8080:8080 \
  -e SANDBOX_API_KEYS=test \
  -e SANDBOX_API_RUNNER_REGISTRATION_TOKEN="${REG_TOKEN}" \
  -e SANDBOX_API_RUNNER_API_KEY="${RUNNER_INTERNAL_API_KEY}" \
  "${API_IMAGE_LOCAL}" >/dev/null

echo "==> Waiting for API healthz ..."
for i in $(seq 1 60); do
  if curl -sf http://localhost:8080/healthz >/dev/null 2>&1; then
    break
  fi
  if [ "$i" -eq 60 ]; then
    echo "API failed to start within 60s"
    docker logs "${API_CONTAINER_NAME}"
    exit 1
  fi
  sleep 1
done

start_runner() {
  local name=$1
  local runner_id=$2
  echo "==> Starting runner container ${name} ..."
  docker run -d \
    "${RUNTIME_ARGS[@]}" \
    --name "${name}" \
    --network "${NETWORK_NAME}" \
    --add-host host.docker.internal:host-gateway \
    -e SANDBOX_RUNNER_API_KEYS="${RUNNER_INTERNAL_API_KEY}" \
    -e SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE="${SANDBOX_IMAGE_REMOTE}" \
    -e SANDBOX_RUNNER_DOCKER_INSECURE_REGISTRIES="${REGISTRY_ADDR}" \
    -e SANDBOX_RUNNER_INTER_SANDBOX_NETWORK_ENABLED=false \
    -e SANDBOX_RUNNER_API_GRPC_ADDR="${API_CONTAINER_NAME}:9090" \
    -e SANDBOX_RUNNER_REGISTRATION_TOKEN="${REG_TOKEN}" \
    -e SANDBOX_RUNNER_HTTP_BASE_URL="http://${name}:8080" \
    -e SANDBOX_RUNNER_ID="${runner_id}" \
    "${RUNNER_IMAGE_LOCAL}" >/dev/null
}

wait_runner_health() {
  local name=$1
  echo "==> Waiting for ${name} healthz ..."
  # AuthMiddleware requires X-Api-Key even for /healthz (same as e2e/run.sh).
  for i in $(seq 1 60); do
    if docker exec "${name}" wget -q -O - --header="X-Api-Key: ${RUNNER_INTERNAL_API_KEY}" http://localhost:8080/healthz >/dev/null 2>&1; then
      echo "==> ${name} is ready."
      return 0
    fi
    if [ "$i" -eq 60 ]; then
      echo "Runner ${name} failed to start within 60s"
      docker logs "${name}"
      exit 1
    fi
    sleep 1
  done
}

# Start runners one at a time: each DinD instance is heavy; parallel starts often miss the 60s window.
start_runner "${RUNNER1_CONTAINER_NAME}" "local-runner-1"
wait_runner_health "${RUNNER1_CONTAINER_NAME}"

start_runner "${RUNNER2_CONTAINER_NAME}" "local-runner-2"
wait_runner_health "${RUNNER2_CONTAINER_NAME}"

sleep 2

echo
echo "API and runners are up."
echo "API:      http://localhost:8080"
echo "Runner 1: ${RUNNER1_CONTAINER_NAME} (internal)"
echo "Runner 2: ${RUNNER2_CONTAINER_NAME} (internal)"
echo
echo "Try:"
echo "curl -s -X POST http://localhost:8080/sandboxes -H 'X-Api-Key: test' | jq"
