#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

export ARCH=$(uname -m | sed 's/aarch64/arm64/' | sed 's/x86_64/amd64/')

echo "==> Building images for ${ARCH} ..."
docker compose --profile build build

COMPOSE_FILES=(-f compose.yaml)
if [[ "$(uname)" == "Darwin" ]]; then
  COMPOSE_FILES+=(-f compose.macos.yaml)
else
  COMPOSE_FILES+=(-f compose.linux.yaml)
fi

echo "==> Starting services ..."
docker compose "${COMPOSE_FILES[@]}" up "$@"
