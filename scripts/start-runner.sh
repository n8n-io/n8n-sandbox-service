#!/bin/sh
set -eu

DOCKERD_LOG=${DOCKERD_LOG:-/tmp/dockerd.log}
DOCKERD_CONFIG_DIR=${DOCKERD_CONFIG_DIR:-/etc/docker}
DOCKERD_CONFIG_FILE=${DOCKERD_CONFIG_FILE:-${DOCKERD_CONFIG_DIR}/daemon.json}

INSECURE_ARGS=""
DOCKERD_ARGS=""
if [ -n "${SANDBOX_RUNNER_DOCKER_INSECURE_REGISTRIES:-}" ]; then
  for reg in $(echo "$SANDBOX_RUNNER_DOCKER_INSECURE_REGISTRIES" | tr ',' ' '); do
    INSECURE_ARGS="$INSECURE_ARGS --insecure-registry $reg"
  done
fi

# Disk-quota storage pool: a loopback xfs+prjquota image used as the inner
# dockerd's data root, so `--storage-opt size=` can cap each sandbox. Only set
# up when the operator has asked for quotas (SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB>0).
# If the kernel lacks xfs quota support (e.g. Docker Desktop linuxkit), the
# mount fails and dockerd starts with default storage (no enforcement).
POOL_PATH=${SANDBOX_RUNNER_DISK_QUOTA_POOL_PATH:-/var/lib/docker.img}
DOCKER_DATA_ROOT=/var/lib/docker
DEFAULT_DISK_QUOTA_MB=${SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB:-0}
CAPACITY_TOTAL=${SANDBOX_RUNNER_CAPACITY_TOTAL:-1000}

# Default pool size = per-sandbox quota × runner capacity × 1.2 (20% headroom
# for sandbox image layers shared across sandboxes), rounded up to whole GB.
# Operators can override with SANDBOX_RUNNER_DISK_QUOTA_POOL_SIZE_GB.
if [ -n "${SANDBOX_RUNNER_DISK_QUOTA_POOL_SIZE_GB:-}" ]; then
  POOL_SIZE_GB=$SANDBOX_RUNNER_DISK_QUOTA_POOL_SIZE_GB
else
  POOL_SIZE_GB=$(( (DEFAULT_DISK_QUOTA_MB * CAPACITY_TOTAL * 12 / 10 + 1023) / 1024 ))
fi

setup_quota_pool() {
  # If there's an existing mount, make sure it uses prjquota.
  existing_mount=$(mount | grep " on ${DOCKER_DATA_ROOT} type xfs " || true)
  if [ -n "$existing_mount" ]; then
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
if [ "$DEFAULT_DISK_QUOTA_MB" -le 0 ]; then
  echo "[start-runner] disk quota enforcement: not requested (SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB unset or 0)"
  if [ -n "${SANDBOX_RUNNER_DOCKER_STORAGE_DRIVER:-}" ]; then
    mkdir -p "$DOCKERD_CONFIG_DIR"
    cat >"$DOCKERD_CONFIG_FILE" <<EOF
{
  "features": {
    "containerd-snapshotter": false
  },
  "storage-driver": "${SANDBOX_RUNNER_DOCKER_STORAGE_DRIVER}"
}
EOF
    echo "[start-runner] dockerd storage driver: ${SANDBOX_RUNNER_DOCKER_STORAGE_DRIVER}"
    echo "[start-runner] dockerd containerd snapshotter: disabled"
    echo "[start-runner] dockerd config file: ${DOCKERD_CONFIG_FILE}"
    DOCKERD_ARGS="$DOCKERD_ARGS --config-file=${DOCKERD_CONFIG_FILE}"
  fi
elif setup_quota_pool; then
  STORAGE_ARGS="--storage-driver=overlay2 --data-root=${DOCKER_DATA_ROOT}"
  export SANDBOX_RUNNER_DISK_QUOTA_ACTIVE=true
  echo "[start-runner] disk quota enforcement: ENABLED (pool ${POOL_SIZE_GB}G, per-sandbox ${DEFAULT_DISK_QUOTA_MB}M)"
else
  echo "[start-runner] disk quota enforcement: DISABLED (kernel lacks xfs quota support or mount failed); sandboxes will use dockerd default storage" >&2
fi

# shellcheck disable=SC2086
dockerd-entrypoint.sh $INSECURE_ARGS $STORAGE_ARGS $DOCKERD_ARGS >"$DOCKERD_LOG" 2>&1 &
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
    --opt "com.docker.network.bridge.enable_icc=false" \
    runner-bridge >/dev/null
fi

/usr/local/bin/sandbox-runner &
RUNNER_PID=$!
wait "$RUNNER_PID"
