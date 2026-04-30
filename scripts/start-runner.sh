#!/bin/sh
set -eu

DOCKERD_LOG=${DOCKERD_LOG:-/tmp/dockerd.log}

INSECURE_ARGS=""
if [ -n "${SANDBOX_RUNNER_DOCKER_INSECURE_REGISTRIES:-}" ]; then
  for reg in $(echo "$SANDBOX_RUNNER_DOCKER_INSECURE_REGISTRIES" | tr ',' ' '); do
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
if ! iptables -n -L DOCKER-USER >/dev/null 2>&1; then
  for bin in iptables-legacy iptables-nft; do
    if command -v "$bin" >/dev/null 2>&1 && "$bin" -n -L DOCKER-USER >/dev/null 2>&1; then
      ln -sf "$(command -v "$bin")" "$(command -v iptables)"
      break
    fi
  done
fi

if [ -z "${SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE:-}" ]; then
  echo "SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE must be set" >&2
  exit 1
fi

if ! docker network inspect runner-bridge >/dev/null 2>&1; then
  docker network create \
    --driver bridge \
    --opt "com.docker.network.bridge.enable_icc=${SANDBOX_RUNNER_INTER_SANDBOX_NETWORK_ENABLED:-false}" \
    runner-bridge >/dev/null
fi

# After `docker stop`/`docker start`, the inner graph usually still has this image; skipping pull
# lets sandbox-runner start immediately so probes (e2e, orchestrators) see /healthz without waiting on the registry.
if ! docker image inspect "${SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE}" >/dev/null 2>&1; then
  docker pull "${SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE}"
fi

exec /usr/local/bin/sandbox-runner
