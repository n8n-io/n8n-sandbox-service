#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

echo "==> Ensuring local gRPC mTLS material (.tls/) ..."
bash scripts/bootstrap-mtls.sh \
    --api-san n8n-sandbox-service-api-local \
    --control-sans "n8n-sandbox-service-runner-dind-local-1,n8n-sandbox-service-runner-dind-local-2,localhost"
if [[ ! -f .tls/api/grpc-server.crt ]] || [[ ! -f .tls/runner/control-grpc-server.crt ]]; then
	echo "Expected .tls/api/grpc-server.crt and .tls/runner/control-grpc-server.crt after bootstrap. Install openssl and retry."
	exit 1
fi
echo "==> Using compose.tls.yaml (mTLS required)."

ARCH=$(uname -m | sed 's/aarch64/arm64/' | sed 's/x86_64/amd64/')
echo "==> Building images for ${ARCH} ..."
ARCH="${ARCH}" docker compose -f compose.yaml --profile build build

echo "==> Starting services ..."
exec bash scripts/docker-compose-local.sh up "$@"
