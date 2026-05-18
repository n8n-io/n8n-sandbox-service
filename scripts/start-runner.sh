#!/bin/sh
set -eu

DOCKERD_LOG=${DOCKERD_LOG:-/tmp/dockerd.log}

INSECURE_ARGS=""
if [ -n "${SANDBOX_RUNNER_DOCKER_INSECURE_REGISTRIES:-}" ]; then
  for reg in $(echo "$SANDBOX_RUNNER_DOCKER_INSECURE_REGISTRIES" | tr ',' ' '); do
    INSECURE_ARGS="$INSECURE_ARGS --insecure-registry $reg"
  done
fi

# Disk-quota storage pool: a loopback xfs+prjquota image used as the inner
# dockerd's data root. When the host kernel supports xfs quotas, dockerd can
# then honor `--storage-opt size=` per sandbox. On hosts without quota support
# (e.g. Docker Desktop's linuxkit kernel) the mount fails and we fall back to
# dockerd's default storage with no per-sandbox enforcement.
POOL_PATH=${SANDBOX_RUNNER_STORAGE_POOL_PATH:-/var/lib/docker.img}
POOL_SIZE_GB=${SANDBOX_RUNNER_STORAGE_POOL_SIZE_GB:-100}
DOCKER_DATA_ROOT=/var/lib/docker

setup_quota_pool() {
  existing_mount=$(mount | grep " on ${DOCKER_DATA_ROOT} type xfs " || true)
  if [ -n "$existing_mount" ]; then
    # prjquota is a mount option, not a filesystem property: a plain xfs mount
    # at this path would let us falsely claim quota enforcement is active while
    # dockerd silently or loudly fails on --storage-opt size=.
    if echo "$existing_mount" | grep -q '[(,]prjquota[,)]'; then
      echo "[start-runner] storage pool already mounted at ${DOCKER_DATA_ROOT} (xfs+prjquota)"
      return 0
    fi
    echo "[start-runner] existing xfs mount at ${DOCKER_DATA_ROOT} lacks prjquota; refusing to claim quota enforcement" >&2
    echo "  mount line: ${existing_mount}" >&2
    return 1
  fi
  if [ ! -f "$POOL_PATH" ]; then
    echo "[start-runner] allocating ${POOL_SIZE_GB}G storage pool at ${POOL_PATH}"
    if ! truncate -s "${POOL_SIZE_GB}G" "$POOL_PATH"; then
      echo "[start-runner] truncate failed for ${POOL_PATH}" >&2
      return 1
    fi
    if ! mkfs.xfs -m crc=1,reflink=1 -L sandboxes -q "$POOL_PATH"; then
      echo "[start-runner] mkfs.xfs failed for ${POOL_PATH}" >&2
      rm -f "$POOL_PATH"
      return 1
    fi
  fi
  mkdir -p "$DOCKER_DATA_ROOT"
  if ! mount -o loop,prjquota "$POOL_PATH" "$DOCKER_DATA_ROOT" 2>/tmp/mount-pool.log; then
    echo "[start-runner] mount with prjquota failed:" >&2
    cat /tmp/mount-pool.log >&2
    return 1
  fi
  echo "[start-runner] storage pool mounted: ${POOL_PATH} on ${DOCKER_DATA_ROOT} (xfs+prjquota)"
  return 0
}

STORAGE_ARGS=""
if setup_quota_pool; then
  STORAGE_ARGS="--storage-driver=overlay2 --data-root=${DOCKER_DATA_ROOT}"
  export SANDBOX_RUNNER_DISK_QUOTA_ACTIVE=true
  echo "[start-runner] disk quota enforcement: ENABLED"
else
  echo "[start-runner] disk quota enforcement: DISABLED (kernel lacks xfs quota support or mount failed); sandboxes will use dockerd default storage" >&2
fi

# shellcheck disable=SC2086
dockerd-entrypoint.sh $INSECURE_ARGS $STORAGE_ARGS >"$DOCKERD_LOG" 2>&1 &
DOCKERD_PID=$!
RUNNER_PID=""

cleanup() {
  if [ -n "$RUNNER_PID" ]; then
    kill "$RUNNER_PID" >/dev/null 2>&1 || true
  fi
  kill "$DOCKERD_PID" >/dev/null 2>&1 || true
  wait >/dev/null 2>&1 || true
}
trap cleanup EXIT
trap 'exit 143' INT TERM

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

/usr/local/bin/sandbox-runner &
RUNNER_PID=$!
wait "$RUNNER_PID"
