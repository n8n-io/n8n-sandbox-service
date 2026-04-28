#!/bin/sh
set -eu

DOCKERD_LOG=${DOCKERD_LOG:-/tmp/dockerd.log}

INSECURE_ARGS=""
if [ -n "${SANDBOX_DOCKER_INSECURE_REGISTRIES:-}" ]; then
  for reg in $(echo "$SANDBOX_DOCKER_INSECURE_REGISTRIES" | tr ',' ' '); do
    INSECURE_ARGS="$INSECURE_ARGS --insecure-registry $reg"
  done
fi

# shellcheck disable=SC2086
dockerd-entrypoint.sh $INSECURE_ARGS >"$DOCKERD_LOG" 2>&1 &
DOCKERD_PID=$!

cleanup() {
  kill "$DOCKERD_PID" >/dev/null 2>&1 || true
  wait "$DOCKERD_PID" >/dev/null 2>&1 || true
}
trap cleanup INT TERM

for _ in $(seq 1 60); do
  if docker info >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

if ! docker info >/dev/null 2>&1; then
  cat "$DOCKERD_LOG"
  exit 1
fi

# Ensure the iptables binary uses the same backend (legacy vs nft) as dockerd.
# dockerd-entrypoint.sh picks a backend and creates the DOCKER-USER chain in it;
# if the default iptables binary points to the other backend, our netrules code
# would write rules into a table that is never traversed.
if ! iptables -n -L DOCKER-USER >/dev/null 2>&1; then
  for bin in iptables-legacy iptables-nft; do
    if command -v "$bin" >/dev/null 2>&1 && "$bin" -n -L DOCKER-USER >/dev/null 2>&1; then
      ln -sf "$(command -v "$bin")" "$(command -v iptables)"
      break
    fi
  done
fi

if [ -z "${SANDBOX_DOCKER_SANDBOX_IMAGE:-}" ]; then
  echo "SANDBOX_DOCKER_SANDBOX_IMAGE must be set" >&2
  exit 1
fi

if ! docker network inspect runner-bridge >/dev/null 2>&1; then
  docker network create \
    --driver bridge \
    --opt "com.docker.network.bridge.enable_icc=${SANDBOX_INTER_SANDBOX_NETWORK_ENABLED:-false}" \
    runner-bridge >/dev/null
fi

docker pull "${SANDBOX_DOCKER_SANDBOX_IMAGE}"

exec /usr/local/bin/sandbox-server
