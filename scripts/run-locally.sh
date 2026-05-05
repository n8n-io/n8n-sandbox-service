#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ "${SANDBOX_COMPOSE_TLS:-1}" != "0" ]]; then
	echo "==> Ensuring local gRPC mTLS material (.tls/) ..."
	bash scripts/bootstrap-local-mtls.sh
	if [[ ! -f .tls/grpc-server.crt ]]; then
		echo "Expected .tls/grpc-server.crt after bootstrap. Install openssl or set SANDBOX_COMPOSE_TLS=0 for plaintext gRPC."
		exit 1
	fi
	echo "==> Using compose.tls.yaml (mTLS). Set SANDBOX_COMPOSE_TLS=0 to disable."
else
	echo "==> SANDBOX_COMPOSE_TLS=0, skipping local mTLS bootstrap."
fi

ARCH=$(uname -m | sed 's/aarch64/arm64/' | sed 's/x86_64/amd64/')
echo "==> Building images for ${ARCH} ..."
ARCH="${ARCH}" docker compose -f compose.yaml --profile build build

echo "==> Starting services ..."
exec bash scripts/docker-compose-local.sh up "$@"
