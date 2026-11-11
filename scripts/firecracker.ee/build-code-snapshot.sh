#!/usr/bin/env bash
# Builds the golden code snapshot (Task API code sandboxes) on a Firecracker
# host, as root:
#   1. rootfs.ext4 from the code sandbox image (CODE_TEMPLATE_DIR)
#   2. a snapshot with code-init as PID 1 and the code daemon ready with the
#      options below (CODE_SNAPSHOT_DIR: snapshot_mem, snapshot_state, boot.json,
#      code.json)
#
# The runner serves code sandboxes from it when
# SANDBOX_RUNNER_FIRECRACKER_CODE_TEMPLATE_DIR and
# SANDBOX_RUNNER_FIRECRACKER_CODE_SNAPSHOT_DIR point at the two directories.
# Creates must use the options baked here.
#
#   CODE_SANDBOX_IMAGE=n8n-code-sandbox:local CODE_INIT_BIN=./code-init \
#     scripts/firecracker.ee/build-code-snapshot.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

CODE_SANDBOX_IMAGE="${CODE_SANDBOX_IMAGE:?set CODE_SANDBOX_IMAGE (the code sandbox image of the n8n repository)}"
CODE_INIT_BIN="${CODE_INIT_BIN:?set CODE_INIT_BIN (static linux build of cmd/code-init.ee)}"
TEMPLATE_DIR="${TEMPLATE_DIR:-/srv/firecracker/template}"
CODE_TEMPLATE_DIR="${CODE_TEMPLATE_DIR:-/srv/firecracker/code-template}"
CODE_SNAPSHOT_DIR="${CODE_SNAPSHOT_DIR:-/srv/firecracker/code-snapshot}"
# Room for 1 GiB of task input plus 1 GiB of output in /work. Sparse.
FIRECRACKER_ROOTFS_SIZE_MB="${FIRECRACKER_ROOTFS_SIZE_MB:-6144}"

# The baked code daemon options. The default is `execution` + `shared`.
CODE_RUNTIME="${CODE_RUNTIME:-n8n-js}"
CODE_SLOTS="${CODE_SLOTS:-2}"
CODE_TASK_MEMORY_MB="${CODE_TASK_MEMORY_MB:-512}"
CODE_SPARE_WORKERS="${CODE_SPARE_WORKERS:-true}"
CODE_REUSE_WORKERS="${CODE_REUSE_WORKERS:-true}"
# Guest memory: slots x task memory + 256 MiB, the memory limit of a Docker
# code sandbox (SANDBOX_RUNNER_CODE_DAEMON_OVERHEAD_MB).
MEM_MIB="${MEM_MIB:-$((CODE_SLOTS * CODE_TASK_MEMORY_MB + 256))}"
VCPUS="${VCPUS:-1}"

if [[ "$(id -u)" -ne 0 ]]; then
	echo "ERROR: run as root" >&2
	exit 1
fi
if [[ ! -f "${TEMPLATE_DIR}/vmlinux" ]]; then
	echo "ERROR: ${TEMPLATE_DIR}/vmlinux not found; build the regular template first" >&2
	exit 1
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

echo "==> Building code rootfs from ${CODE_SANDBOX_IMAGE}..."
FIRECRACKER_ROOTFS_SIZE_MB="$FIRECRACKER_ROOTFS_SIZE_MB" \
	"${SCRIPT_DIR}/build-rootfs-template.sh" \
	--kernel "${TEMPLATE_DIR}/vmlinux" \
	--sandbox-image "$CODE_SANDBOX_IMAGE" \
	--template-dir "$CODE_TEMPLATE_DIR"

cat >"${work}/code-daemon.env" <<EOF
CODE_DAEMON_RUNTIME=${CODE_RUNTIME}
CODE_DAEMON_SLOTS=${CODE_SLOTS}
CODE_DAEMON_TASK_MEMORY_MB=${CODE_TASK_MEMORY_MB}
CODE_DAEMON_SPARE_WORKERS=${CODE_SPARE_WORKERS}
CODE_DAEMON_REUSE_WORKERS=${CODE_REUSE_WORKERS}
EOF
cat >"${work}/code.json" <<EOF
{"runtime":"${CODE_RUNTIME}","slots":${CODE_SLOTS},"taskMemoryMb":${CODE_TASK_MEMORY_MB},"spareWorkers":${CODE_SPARE_WORKERS},"reuseWorkers":${CODE_REUSE_WORKERS}}
EOF

echo "==> Creating golden code snapshot in ${CODE_SNAPSHOT_DIR} (${MEM_MIB} MiB, ${VCPUS} vCPU)..."
mkdir -p "$CODE_SNAPSHOT_DIR"
MEM_MIB="$MEM_MIB" VCPUS="$VCPUS" \
	"${SCRIPT_DIR}/create-golden-snapshot.sh" \
	--kernel "${CODE_TEMPLATE_DIR}/vmlinux" \
	--ext4 "${CODE_TEMPLATE_DIR}/rootfs.ext4" \
	--code-init "$CODE_INIT_BIN" \
	--code-env "${work}/code-daemon.env" \
	--code-options "${work}/code.json" \
	--out "$CODE_SNAPSHOT_DIR"
# All code sandboxes map these files directly (read-only, not copied), so no
# host process of uid 1000 (the jailer user) may change them.
chown root:root "${CODE_SNAPSHOT_DIR}"/snapshot_mem "${CODE_SNAPSHOT_DIR}"/snapshot_state "${CODE_SNAPSHOT_DIR}"/*.json
chmod 0644 "${CODE_SNAPSHOT_DIR}"/snapshot_mem "${CODE_SNAPSHOT_DIR}"/snapshot_state "${CODE_SNAPSHOT_DIR}"/*.json

echo "==> Golden code snapshot ready"
echo "SANDBOX_RUNNER_FIRECRACKER_CODE_TEMPLATE_DIR=${CODE_TEMPLATE_DIR}"
echo "SANDBOX_RUNNER_FIRECRACKER_CODE_SNAPSHOT_DIR=${CODE_SNAPSHOT_DIR}"
