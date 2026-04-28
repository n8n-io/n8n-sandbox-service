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
RUNNER_CONTAINER_NAME=${N8N_SANDBOX_RUNNER_CONTAINER_NAME:-n8n-sandbox-runner-local}
NETWORK_NAME=${N8N_SANDBOX_LOCAL_NETWORK:-n8n-sandbox-local}
RUNNER_INTERNAL_API_KEY=${N8N_SANDBOX_RUNNER_API_KEY:-runner-local-key}

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
docker rm -f "${RUNNER_CONTAINER_NAME}" >/dev/null 2>&1 || true

RUNTIME_ARGS=()
if [[ "$(uname)" == "Darwin" ]]; then
  echo "==> Running runner container with --privileged (macOS, no sysbox) ..."
  RUNTIME_ARGS+=(--privileged)
else
  echo "==> Running runner container with sysbox ..."
  RUNTIME_ARGS+=(--runtime=sysbox-runc)
fi

echo "==> Starting runner container ..."
docker run -d \
  "${RUNTIME_ARGS[@]}" \
  --name "${RUNNER_CONTAINER_NAME}" \
  --network "${NETWORK_NAME}" \
  --add-host host.docker.internal:host-gateway \
  -e SANDBOX_API_KEYS="${RUNNER_INTERNAL_API_KEY}" \
  -e SANDBOX_DOCKER_SANDBOX_IMAGE="${SANDBOX_IMAGE_REMOTE}" \
  -e SANDBOX_DOCKER_INSECURE_REGISTRIES="${REGISTRY_ADDR}" \
  -e SANDBOX_INTER_SANDBOX_NETWORK_ENABLED=false \
  "${RUNNER_IMAGE_LOCAL}" >/dev/null

echo "==> Waiting for runner healthz ..."
for i in $(seq 1 60); do
  if docker exec "${RUNNER_CONTAINER_NAME}" wget -q -O - http://localhost:8080/healthz >/dev/null 2>&1; then
    break
  fi
  if [ "$i" -eq 60 ]; then
    echo "Runner failed to start within 60s"
    docker logs "${RUNNER_CONTAINER_NAME}"
    exit 1
  fi
  sleep 1
done

echo "==> Starting API container on localhost:8080 ..."
docker run -d \
  --name "${API_CONTAINER_NAME}" \
  --network "${NETWORK_NAME}" \
  -p 8080:8080 \
  -e SANDBOX_API_KEYS=test \
  -e SANDBOX_RUNNER_URL="http://${RUNNER_CONTAINER_NAME}:8080" \
  -e SANDBOX_RUNNER_API_KEY="${RUNNER_INTERNAL_API_KEY}" \
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

echo
echo "API and runner are up."
echo "API:    http://localhost:8080"
echo "Runner: ${RUNNER_CONTAINER_NAME} (internal only)"
echo
echo "Try:"
echo "curl -s -X POST http://localhost:8080/sandboxes -H 'X-Api-Key: test' | jq"
