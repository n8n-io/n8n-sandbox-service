#!/usr/bin/env bash
# Restores a Firecracker snapshot and verifies guest daemon health.
set -euo pipefail

VARIANT="${1:-none}"
SNAPSHOT_DIR="${2:?snapshot directory required}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="${PROJECT_ROOT:-$(cd "$SCRIPT_DIR/../.." && pwd)}"

KERNEL="/srv/firecracker/template/vmlinux"
ROOTFS="/srv/firecracker/template/rootfs.ext4"
MEM_MIB="${MEM_MIB:-512}"
VCPUS="${VCPUS:-1}"
FIRECRACKER_BIN="${FIRECRACKER_BIN:-/opt/firecracker/bin/firecracker}"
JAILER_BIN="${JAILER_BIN:-/opt/firecracker/bin/jailer}"
JAILER_BASE_DIR="${JAILER_BASE_DIR:-/srv/jailer}"
GUEST_IP="${GUEST_IP:-172.16.0.10}"
HOST_TAP_IP_CIDR="${HOST_TAP_IP_CIDR:-172.16.0.1/24}"
HOST_TAP_IP="${HOST_TAP_IP_CIDR%%/*}"
HOST_TAP_DEVICE_NAME="${HOST_TAP_DEVICE_NAME:-fc-tap-0}"
DAEMON_PORT="${DAEMON_PORT:-8081}"
GUEST_MAC="${GUEST_MAC:-AA:FC:00:00:00:01}"
VM_ID="restore-$RANDOM-$$"
NETNS="fc-restore-$$"
JAIL_ROOT=""
FC_PID=""

SNAPSHOT_DIR="$(realpath "$SNAPSHOT_DIR")"
SNAPSHOT_MEM="${SNAPSHOT_DIR}/snapshot_mem"
SNAPSHOT_STATE="${SNAPSHOT_DIR}/snapshot_state"

if [[ ! -f "$SNAPSHOT_MEM" || ! -f "$SNAPSHOT_STATE" ]]; then
	echo "ERROR: snapshot files not found in ${SNAPSHOT_DIR}" >&2
	exit 1
fi

CPU_CONFIG_FILE=""
case "$VARIANT" in
none | T2 | T2S | T2CL | C3) ;;
helper-custom | helper-intel-only)
	CPU_CONFIG_FILE="/tmp/fc-compat-cpu-configs/${VARIANT}.json"
	if [[ ! -f "$CPU_CONFIG_FILE" ]]; then
		CPU_CONFIG_FILE="$(bash "${SCRIPT_DIR}/lib/resolve-helper-config.sh" "$VARIANT")"
	fi
	;;
no-xcrs)
	CPU_CONFIG_FILE="/tmp/fc-compat-cpu-configs/no-xcrs.json"
	;;
*)
	echo "unknown variant: $VARIANT" >&2
	exit 1
	;;
esac

if [[ "$VARIANT" == "no-xcrs" && ! -f "$CPU_CONFIG_FILE" ]]; then
	mkdir -p "$(dirname "$CPU_CONFIG_FILE")"
	cat >"$CPU_CONFIG_FILE" <<'EOF'
{
  "kvm_capabilities": ["!56"]
}
EOF
fi
api_put() {
	local path=$1 body=$2
	curl --fail-with-body --silent --show-error \
		--unix-socket "${JAIL_ROOT}/firecracker.socket" \
		-X PUT "http://localhost${path}" \
		-H "Content-Type: application/json" \
		-d "$body" >/dev/null
}

cleanup() {
	local exit_code=$?
	set +e
	if [[ -n "$FC_PID" ]]; then
		kill "$FC_PID" >/dev/null 2>&1
		wait "$FC_PID" >/dev/null 2>&1
	fi
	if [[ -n "$JAIL_ROOT" ]]; then
		umount -l "${JAIL_ROOT}/vmlinux" >/dev/null 2>&1
		umount -l "${JAIL_ROOT}/rootfs.ext4" >/dev/null 2>&1
		umount -l "${JAIL_ROOT}/snapshot_mem" >/dev/null 2>&1
		umount -l "${JAIL_ROOT}/snapshot_state" >/dev/null 2>&1
	fi
	ip netns delete "$NETNS" >/dev/null 2>&1
	rm -rf "${JAILER_BASE_DIR}/firecracker/${VM_ID}" >/dev/null 2>&1
	exit "$exit_code"
}
trap cleanup EXIT

ip netns add "$NETNS"
ip netns exec "$NETNS" ip link set lo up
ip netns exec "$NETNS" ip tuntap add name "$HOST_TAP_DEVICE_NAME" mode tap
ip netns exec "$NETNS" ip addr add "$HOST_TAP_IP_CIDR" dev "$HOST_TAP_DEVICE_NAME"
ip netns exec "$NETNS" ip link set "$HOST_TAP_DEVICE_NAME" up

"$JAILER_BIN" \
	--id "$VM_ID" \
	--exec-file "$FIRECRACKER_BIN" \
	--uid 1000 \
	--gid 1000 \
	--chroot-base-dir "$JAILER_BASE_DIR" \
	--netns "/run/netns/${NETNS}" \
	-- \
	--api-sock /firecracker.socket &
FC_PID=$!

JAIL_ROOT="${JAILER_BASE_DIR}/firecracker/${VM_ID}/root"
for _ in $(seq 1 200); do
	if [[ -S "${JAIL_ROOT}/firecracker.socket" ]]; then
		break
	fi
	sleep 0.05
done
if [[ ! -S "${JAIL_ROOT}/firecracker.socket" ]]; then
	echo "ERROR: timed out waiting for Firecracker API socket" >&2
	exit 1
fi

touch "${JAIL_ROOT}/vmlinux" "${JAIL_ROOT}/rootfs.ext4" "${JAIL_ROOT}/snapshot_mem" "${JAIL_ROOT}/snapshot_state"
mount --bind "$KERNEL" "${JAIL_ROOT}/vmlinux"
mount --bind "$ROOTFS" "${JAIL_ROOT}/rootfs.ext4"
mount --bind "$SNAPSHOT_MEM" "${JAIL_ROOT}/snapshot_mem"
mount --bind "$SNAPSHOT_STATE" "${JAIL_ROOT}/snapshot_state"

# Full snapshots embed machine/boot config. Only /cpu-config may be set before load
# (for custom template variants). Never call /machine-config before /snapshot/load.
if [[ -n "$CPU_CONFIG_FILE" && -f "$CPU_CONFIG_FILE" ]]; then
	api_put "/cpu-config" "$(cat "$CPU_CONFIG_FILE")"
fi

api_put "/snapshot/load" "$(cat <<EOF
{
  "snapshot_path": "/snapshot_state",
  "mem_backend": {
    "backend_type": "File",
    "backend_path": "/snapshot_mem"
  },
  "track_dirty_pages": false,
  "resume_vm": true
}
EOF
)"

for _ in $(seq 1 120); do
	if ip netns exec "$NETNS" curl -fsS --max-time 1 "http://${GUEST_IP}:${DAEMON_PORT}/healthz" >/dev/null 2>&1; then
		echo "restore_ok"
		exit 0
	fi
	sleep 0.5
done

echo "ERROR: sandbox daemon did not become healthy after snapshot restore" >&2
exit 1
