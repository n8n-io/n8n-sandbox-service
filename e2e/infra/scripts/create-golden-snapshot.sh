#!/usr/bin/env bash
# Builds the Firecracker snapshot used by the e2e runner on the target VM.
# Intended to run as root from the repository root.
set -euo pipefail

KERNEL=""
ROOTFS=""
DAEMON_BIN=""
OUT_DIR=""
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
VM_ID="snapshot-$RANDOM-$$"
NETNS="fc-snapshot-$$"
JAIL_ROOT=""
FC_PID=""
ROOTFS_MOUNT=""

# Prints the required CLI arguments for the snapshot helper.
usage() {
	cat >&2 <<EOF
Usage: $0 --kernel PATH --ext4 PATH --daemon-bin PATH --out DIR
EOF
}

while [[ $# -gt 0 ]]; do
	case "$1" in
	--kernel)
		KERNEL="$2"
		shift 2
		;;
	--ext4)
		ROOTFS="$2"
		shift 2
		;;
	--daemon-bin)
		DAEMON_BIN="$2"
		shift 2
		;;
	--out)
		OUT_DIR="$2"
		shift 2
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		usage
		echo "unknown argument: $1" >&2
		exit 1
		;;
	esac
done

if [[ -z "$KERNEL" || -z "$ROOTFS" || -z "$DAEMON_BIN" || -z "$OUT_DIR" ]]; then
	usage
	exit 1
fi

KERNEL="$(realpath "$KERNEL")"
ROOTFS="$(realpath "$ROOTFS")"
DAEMON_BIN="$(realpath "$DAEMON_BIN")"
OUT_DIR="$(realpath "$OUT_DIR")"
SNAPSHOT_MEM="${OUT_DIR}/snapshot_mem"
SNAPSHOT_STATE="${OUT_DIR}/snapshot_state"

# Sends a PUT request to the Firecracker API socket in the jail. Firecracker uses
# REST over a Unix socket for machine configuration and lifecycle actions.
api_put() {
	local path=$1 body=$2
	curl --fail-with-body --silent --show-error \
		--unix-socket "${JAIL_ROOT}/firecracker.socket" \
		-X PUT "http://localhost${path}" \
		-H "Content-Type: application/json" \
		-d "$body" >/dev/null
}

# Sends a PATCH request to the Firecracker API socket. Snapshot creation pauses
# the VM through PATCH /vm before writing the full snapshot.
api_patch() {
	local path=$1 body=$2
	curl --fail-with-body --silent --show-error \
		--unix-socket "${JAIL_ROOT}/firecracker.socket" \
		-X PATCH "http://localhost${path}" \
		-H "Content-Type: application/json" \
		-d "$body" >/dev/null
}

# Best-effort cleanup for all host resources created by this helper. It must be
# safe after partial setup because failures can happen while mounts/netns exist.
cleanup() {
	local exit_code=$?
	set +e
	if [[ -n "$FC_PID" ]]; then
		kill "$FC_PID" >/dev/null 2>&1
		wait "$FC_PID" >/dev/null 2>&1
	fi
	if [[ -n "$ROOTFS_MOUNT" ]]; then
		umount -l "$ROOTFS_MOUNT" >/dev/null 2>&1
		rmdir "$ROOTFS_MOUNT" >/dev/null 2>&1
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

echo "==> Installing sandbox daemon into Firecracker rootfs..."
ROOTFS_MOUNT="$(mktemp -d)"
mount -o loop "$ROOTFS" "$ROOTFS_MOUNT"
install -m 0755 "$DAEMON_BIN" "${ROOTFS_MOUNT}/sandbox-daemon"
sync
umount "$ROOTFS_MOUNT"
rmdir "$ROOTFS_MOUNT"
ROOTFS_MOUNT=""

echo "==> Preparing snapshot network namespace..."
ip netns add "$NETNS"
ip netns exec "$NETNS" ip link set lo up
ip netns exec "$NETNS" ip tuntap add name "$HOST_TAP_DEVICE_NAME" mode tap
ip netns exec "$NETNS" ip addr add "$HOST_TAP_IP_CIDR" dev "$HOST_TAP_DEVICE_NAME"
ip netns exec "$NETNS" ip link set "$HOST_TAP_DEVICE_NAME" up

echo "==> Starting Firecracker for snapshot creation..."
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

mkdir -p "$OUT_DIR"
rm -f "$SNAPSHOT_MEM" "$SNAPSHOT_STATE"
touch "$SNAPSHOT_MEM" "$SNAPSHOT_STATE"
chown 1000:1000 "$SNAPSHOT_MEM" "$SNAPSHOT_STATE"
touch "${JAIL_ROOT}/vmlinux" "${JAIL_ROOT}/rootfs.ext4" "${JAIL_ROOT}/snapshot_mem" "${JAIL_ROOT}/snapshot_state"
mount --bind "$KERNEL" "${JAIL_ROOT}/vmlinux"
mount --bind "$ROOTFS" "${JAIL_ROOT}/rootfs.ext4"
mount --bind "$SNAPSHOT_MEM" "${JAIL_ROOT}/snapshot_mem"
mount --bind "$SNAPSHOT_STATE" "${JAIL_ROOT}/snapshot_state"

boot_args="console=ttyS0 reboot=k panic=1 pci=off init=/sandbox-daemon ip=${GUEST_IP}::${HOST_TAP_IP}:255.255.255.0::eth0:off"

echo "==> Configuring and booting Firecracker snapshot VM..."
api_put "/machine-config" "{\"vcpu_count\":${VCPUS},\"mem_size_mib\":${MEM_MIB},\"smt\":false}"
api_put "/boot-source" "{\"kernel_image_path\":\"/vmlinux\",\"boot_args\":\"${boot_args}\"}"
api_put "/drives/rootfs" "{\"drive_id\":\"rootfs\",\"path_on_host\":\"/rootfs.ext4\",\"is_root_device\":true,\"is_read_only\":false}"
api_put "/network-interfaces/eth0" "{\"iface_id\":\"eth0\",\"guest_mac\":\"${GUEST_MAC}\",\"host_dev_name\":\"${HOST_TAP_DEVICE_NAME}\"}"
api_put "/actions" '{"action_type":"InstanceStart"}'

echo "==> Waiting for sandbox daemon inside snapshot VM..."
for _ in $(seq 1 120); do
	if ip netns exec "$NETNS" curl -fsS --max-time 1 "http://${GUEST_IP}:${DAEMON_PORT}/healthz" >/dev/null 2>&1; then
		break
	fi
	sleep 0.5
done
if ! ip netns exec "$NETNS" curl -fsS --max-time 2 "http://${GUEST_IP}:${DAEMON_PORT}/healthz" >/dev/null; then
	echo "ERROR: sandbox daemon did not become healthy before snapshot" >&2
	exit 1
fi

echo "==> Pausing VM and writing Firecracker snapshot..."
api_patch "/vm" '{"state":"Paused"}'
api_put "/snapshot/create" '{"snapshot_type":"Full","snapshot_path":"/snapshot_state","mem_file_path":"/snapshot_mem"}'

chmod 0644 "$SNAPSHOT_MEM" "$SNAPSHOT_STATE"
echo "==> Firecracker snapshot created at ${OUT_DIR}"
