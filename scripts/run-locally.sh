#!/usr/bin/env bash
set -euo pipefail

ARCH=$(uname -m | sed 's/aarch64/arm64/' | sed 's/x86_64/amd64/')
REGISTRY_NAME=${N8N_SANDBOX_REGISTRY_NAME:-n8n-sandbox-registry}
REGISTRY_ADDR=${N8N_SANDBOX_REGISTRY_ADDR:-host.docker.internal:5050}
SERVICE_IMAGE_LOCAL="n8n-sandbox-service:latest-${ARCH}"
SANDBOX_IMAGE_LOCAL="n8n-sandbox:latest-${ARCH}"
REGISTRY_PORT=${REGISTRY_ADDR##*:}
SANDBOX_IMAGE_PUSH="localhost:${REGISTRY_PORT}/n8n-sandbox:latest-${ARCH}"
SANDBOX_IMAGE_REMOTE="${REGISTRY_ADDR}/n8n-sandbox:latest-${ARCH}"
CONTAINER_NAME=${N8N_SANDBOX_CONTAINER_NAME:-n8n-sandbox-local}

echo "==> Building service and sandbox images for ${ARCH} ..."
make docker-local

if ! docker ps --format '{{.Names}}' | grep -qx "${REGISTRY_NAME}"; then
  echo "==> Starting local registry ${REGISTRY_NAME} on port 5050 ..."
  docker rm -f "${REGISTRY_NAME}" >/dev/null 2>&1 || true
  docker run -d --restart unless-stopped --name "${REGISTRY_NAME}" -p 5050:5000 registry:2 >/dev/null
fi

echo "==> Pushing sandbox image to local registry ..."
docker tag "${SANDBOX_IMAGE_LOCAL}" "${SANDBOX_IMAGE_PUSH}"
docker push "${SANDBOX_IMAGE_PUSH}"

docker rm -f "${CONTAINER_NAME}" >/dev/null 2>&1 || true

RUNTIME_ARGS=()
if [[ "$(uname)" == "Darwin" ]]; then
  echo "==> Running service container with --privileged (macOS, no sysbox) ..."
  RUNTIME_ARGS+=(--privileged)
else
  echo "==> Running service container with sysbox ..."
  RUNTIME_ARGS+=(--runtime=sysbox-runc)
fi

docker run --rm -it \
  "${RUNTIME_ARGS[@]}" \
  --name "${CONTAINER_NAME}" \
  --add-host host.docker.internal:host-gateway \
  -p 8080:8080 \
  -e SANDBOX_API_KEYS=test \
  -e SANDBOX_DOCKER_SANDBOX_IMAGE="${SANDBOX_IMAGE_REMOTE}" \
  -e SANDBOX_DOCKER_INSECURE_REGISTRIES="${REGISTRY_ADDR}" \
  -e SANDBOX_INTER_SANDBOX_NETWORK_ENABLED=false \
  "${SERVICE_IMAGE_LOCAL}"
