#!/usr/bin/env bash
# Local docker compose with ARCH set and the same -f stack as scripts/run-locally.sh.
set -euo pipefail
cd "$(dirname "$0")/.."

export ARCH
ARCH=$(uname -m | sed 's/aarch64/arm64/' | sed 's/x86_64/amd64/')

COMPOSE_FILES=(-f compose.yaml)
if [[ "$(uname)" == "Darwin" ]]; then
	COMPOSE_FILES+=(-f compose.macos.yaml)
else
	COMPOSE_FILES+=(-f compose.linux.yaml)
fi

if [[ "${SANDBOX_COMPOSE_TLS:-1}" != "0" ]]; then
	COMPOSE_FILES+=(-f compose.tls.yaml)
fi

exec docker compose "${COMPOSE_FILES[@]}" "$@"
