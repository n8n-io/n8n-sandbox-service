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
TLS_DIR="${E2E_TLS_DIR:-$(mktemp -d)}"
TLS_DIR_OWNED=0
if [[ -z "${E2E_TLS_DIR:-}" ]]; then
  TLS_DIR_OWNED=1
fi
API_TLS_DNS="${E2E_API_TLS_DNS:-sandbox-api-e2e-mtls}"

normalize_tls_permissions() {
  chmod 755 "$TLS_DIR"
  for f in "$TLS_DIR"/*.crt "$TLS_DIR"/*.key; do
    [[ -e "$f" ]] || continue
    chmod 644 "$f"
  done
}

cleanup() {
  echo "Stopping e2e resources..."
  docker stop "$API_CONTAINER_NAME" >/dev/null 2>&1 || true
  docker rm "$API_CONTAINER_NAME" >/dev/null 2>&1 || true
  docker network rm "$NETWORK_NAME" >/dev/null 2>&1 || true
  if [[ "$TLS_DIR_OWNED" == "1" ]]; then
    rm -rf "$TLS_DIR"
  fi
}
trap cleanup EXIT

if [[ "${E2E_SKIP_BUILD:-}" != "1" ]]; then
  echo "Building API image..."
  make -C "$PROJECT_DIR" "docker-api-${ARCH}"
fi

if [[ "$TLS_DIR_OWNED" == "1" ]]; then
  echo "Bootstrapping e2e mTLS material..."
  OUT_DIR="$TLS_DIR" API_DNS="$API_TLS_DNS" CONTROL_SANS="runner-control" \
    bash "$PROJECT_DIR/scripts/bootstrap-local-mtls.sh"
else
  echo "Using shared e2e mTLS material from E2E_TLS_DIR..."
fi
normalize_tls_permissions

docker network create "$NETWORK_NAME" >/dev/null

echo "Starting API only on port $PORT..."
docker run -d \
  --network "$NETWORK_NAME" \
  -p "$PORT:8080" \
  -v "$TLS_DIR:/grpc-tls:ro" \
  -e "SANDBOX_API_KEYS=$API_KEY" \
  -e "SANDBOX_API_RUNNER_REGISTRATION_TOKEN=$REG_TOKEN" \
  -e "SANDBOX_API_GRPC_TLS_CERT_FILE=/grpc-tls/grpc-server.crt" \
  -e "SANDBOX_API_GRPC_TLS_KEY_FILE=/grpc-tls/grpc-server.key" \
  -e "SANDBOX_API_GRPC_TLS_CLIENT_CA_FILE=/grpc-tls/ca.crt" \
  -e "SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CA_FILE=/grpc-tls/ca.crt" \
  -e "SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CERT_FILE=/grpc-tls/control-grpc-api-client.crt" \
  -e "SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_KEY_FILE=/grpc-tls/control-grpc-api-client.key" \
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
if [ ! -d node_modules ] || [ ! -f node_modules/@n8n/sandbox-client/dist/index.js ]; then
  echo "Installing dependencies..."
  pnpm install --frozen-lockfile
fi

echo "Running no-runner placement test..."
BASE_URL="http://localhost:$PORT" SANDBOX_API_KEY="$API_KEY" npx playwright test tests/placement-no-runners.spec.ts "$@"
