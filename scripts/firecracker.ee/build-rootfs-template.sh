#!/usr/bin/env bash
# Builds the Firecracker rootfs template (rootfs.ext4 + vmlinux) from a sandbox
# OCI image (same userspace as Dockerfile.sandbox) plus a Firecracker CI kernel.
# Used by e2e VM setup, golden-build bundle consumers, and gallery bake.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FIRECRACKER_CI_ASSETS_BIN="${FIRECRACKER_CI_ASSETS_BIN:-${SCRIPT_DIR}/firecracker-ci-assets.sh}"

FIRECRACKER_CI_VERSION="${FIRECRACKER_CI_VERSION:-v1.14}"
FIRECRACKER_CI_VMLINUX="${FIRECRACKER_CI_VMLINUX:-}"
TEMPLATE_DIR="${TEMPLATE_DIR:-/srv/firecracker/template}"
FIRECRACKER_ROOTFS_SIZE_MB="${FIRECRACKER_ROOTFS_SIZE_MB:-${FIRECRACKER_E2E_ROOTFS_SIZE_MB:-2048}}"
# Guest uid 1000 cannot use mkfs reserved blocks. Format with -m 0 and require
# this much free space after journal/inodes for /tmp, workspace, and the
# snapshot-time sandbox-daemon install.
ROOTFS_MIN_FREE_MB=128
SANDBOX_IMAGE="${SANDBOX_IMAGE:-}"
SANDBOX_ROOTFS_TAR="${SANDBOX_ROOTFS_TAR:-}"

usage() {
	cat >&2 <<EOF
Usage: $0 [options]

Builds rootfs.ext4 from a sandbox OCI image (or pre-exported filesystem tar) and
installs vmlinux into TEMPLATE_DIR.

Options:
  --kernel PATH           Kernel image (FIRECRACKER_CI_VMLINUX)
  --sandbox-image REF     OCI image ref to unpack (SANDBOX_IMAGE)
  --sandbox-rootfs-tar P  Pre-exported rootfs tar (SANDBOX_ROOTFS_TAR)
  --template-dir DIR      Output directory (TEMPLATE_DIR)
  -h, --help              Show this help

Provide exactly one of --sandbox-image or --sandbox-rootfs-tar (or the matching
env vars). When --kernel is omitted, downloads the Firecracker CI vmlinux using
FIRECRACKER_CI_VERSION.

Unpack requires crane or docker on PATH when using --sandbox-image.
EOF
}

maybe_sudo() {
	if [[ "$(id -u)" -eq 0 ]]; then
		"$@"
	else
		sudo "$@"
	fi
}

require_elf_kernel() {
	local path=$1
	if ! LC_ALL=C od -An -N4 -tx1 "$path" | tr -d ' \n' | grep -qi '^7f454c46$'; then
		echo "ERROR: kernel must be an uncompressed ELF vmlinux image: $path" >&2
		exit 1
	fi
}

verify_rootfs_resolv_conf() {
	local ext4_path=$1 resolv
	if ! command -v debugfs >/dev/null 2>&1; then
		echo "WARN: debugfs not available; skipping resolv.conf verification" >&2
		return 0
	fi
	resolv="$(debugfs -R 'cat /etc/resolv.conf' "$ext4_path" 2>/dev/null || true)"
	if ! grep -q 'nameserver 8.8.8.8' <<<"$resolv" || ! grep -q 'nameserver 1.1.1.1' <<<"$resolv"; then
		echo "ERROR: built rootfs.ext4 is missing seeded nameservers in /etc/resolv.conf" >&2
		echo "resolv.conf contents:" >&2
		printf '%s\n' "$resolv" >&2
		exit 1
	fi
}

seed_resolv_conf() {
	local rootfs_dir=$1
	# Container images often ship resolv.conf as a symlink (Docker DNS / systemd);
	# tee would follow it and write outside the staged tree.
	maybe_sudo rm -f "$rootfs_dir/etc/resolv.conf"
	printf 'nameserver 8.8.8.8\nnameserver 1.1.1.1\n' | maybe_sudo tee "$rootfs_dir/etc/resolv.conf" >/dev/null
}

ensure_sandbox_user() {
	local rootfs_dir=$1
	if ! maybe_sudo grep -q '^user:' "$rootfs_dir/etc/group"; then
		echo 'user:x:1000:' | maybe_sudo tee -a "$rootfs_dir/etc/group" >/dev/null
	fi
	if ! maybe_sudo grep -q '^user:' "$rootfs_dir/etc/passwd"; then
		echo 'user:x:1000:1000:Sandbox User:/home/user:/bin/sh' | maybe_sudo tee -a "$rootfs_dir/etc/passwd" >/dev/null
	fi
	maybe_sudo install -d -m 0755 -o 1000 -g 1000 "$rootfs_dir/home/user"
	# Caller may chown the staged tree to root for a consistent rootfs; restore the
	# sandbox home so daemon (drops to uid 1000) can write workspace/skills/etc.
	maybe_sudo chown -R 1000:1000 "$rootfs_dir/home/user"
	maybe_sudo install -d -m 1777 -o root -g root "$rootfs_dir/tmp"
}

unpack_sandbox_image() {
	local image=$1 dest=$2
	local cid

	mkdir -p "$dest"
	if command -v crane >/dev/null 2>&1; then
		echo "==> Exporting ${image} with crane..."
		crane export "$image" - | tar -x -C "$dest"
		return 0
	fi
	if command -v docker >/dev/null 2>&1; then
		local docker_bin=(docker)
		if ! docker info >/dev/null 2>&1 && command -v sudo >/dev/null 2>&1; then
			docker_bin=(sudo docker)
		fi
		echo "==> Exporting ${image} with ${docker_bin[*]}..."
		cid="$("${docker_bin[@]}" create "$image")"
		if ! "${docker_bin[@]}" export "$cid" | tar -x -C "$dest"; then
			"${docker_bin[@]}" rm -f "$cid" >/dev/null 2>&1 || true
			echo "ERROR: docker export failed for ${image}" >&2
			exit 1
		fi
		"${docker_bin[@]}" rm -f "$cid" >/dev/null
		return 0
	fi
	echo "ERROR: unpacking SANDBOX_IMAGE requires crane or docker on PATH" >&2
	exit 1
}

unpack_sandbox_rootfs_tar() {
	local tar_path=$1 dest=$2
	mkdir -p "$dest"
	echo "==> Extracting sandbox rootfs tar ${tar_path}..."
	tar -x -C "$dest" -f "$tar_path"
}

ext4_free_mb() {
	local info block_size free_blocks
	info="$(maybe_sudo tune2fs -l "$1")"
	block_size="$(awk -F: '/^Block size:/ {gsub(/[[:space:]]/, "", $2); print $2}' <<<"$info")"
	free_blocks="$(awk -F: '/^Free blocks:/ {gsub(/[[:space:]]/, "", $2); print $2}' <<<"$info")"
	if [[ -z "$block_size" || -z "$free_blocks" ]]; then
		echo "ERROR: could not read ext4 free space from $1" >&2
		exit 1
	fi
	echo $((free_blocks * block_size / 1024 / 1024))
}

while [[ $# -gt 0 ]]; do
	case "$1" in
	--kernel)
		FIRECRACKER_CI_VMLINUX="$2"
		shift 2
		;;
	--sandbox-image)
		SANDBOX_IMAGE="$2"
		shift 2
		;;
	--sandbox-rootfs-tar)
		SANDBOX_ROOTFS_TAR="$2"
		shift 2
		;;
	--template-dir)
		TEMPLATE_DIR="$2"
		shift 2
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		echo "unknown argument: $1" >&2
		usage
		exit 1
		;;
	esac
done

if [[ "$(uname -m)" != "x86_64" ]]; then
	echo "Firecracker rootfs template build supports amd64/x86_64 only; got $(uname -m)" >&2
	exit 1
fi

if [[ -n "$SANDBOX_IMAGE" && -n "$SANDBOX_ROOTFS_TAR" ]]; then
	echo "ERROR: provide only one of SANDBOX_IMAGE / SANDBOX_ROOTFS_TAR" >&2
	exit 1
fi
if [[ -z "$SANDBOX_IMAGE" && -z "$SANDBOX_ROOTFS_TAR" ]]; then
	echo "ERROR: set SANDBOX_IMAGE or SANDBOX_ROOTFS_TAR (sandbox OCI userspace source)" >&2
	usage
	exit 1
fi

work="$(mktemp -d)"
rootfs_dir="${work}/rootfs"
ext4_path="${work}/rootfs.ext4"
cleanup() {
	maybe_sudo rm -rf "$work"
}
trap cleanup EXIT

if [[ -z "$FIRECRACKER_CI_VMLINUX" ]]; then
	ci_assets_dir="${work}/ci-assets"
	if [[ ! -x "$FIRECRACKER_CI_ASSETS_BIN" ]]; then
		echo "ERROR: missing firecracker-ci-assets script: ${FIRECRACKER_CI_ASSETS_BIN}" >&2
		exit 1
	fi
	FIRECRACKER_CI_VERSION="$FIRECRACKER_CI_VERSION" \
		"$FIRECRACKER_CI_ASSETS_BIN" download "$ci_assets_dir"
	# shellcheck disable=SC1090
	source "${ci_assets_dir}/manifest.env"
fi

if [[ ! -f "$FIRECRACKER_CI_VMLINUX" ]]; then
	echo "ERROR: kernel not found: $FIRECRACKER_CI_VMLINUX" >&2
	exit 1
fi

require_elf_kernel "$FIRECRACKER_CI_VMLINUX"

echo "==> Building Firecracker rootfs template in ${TEMPLATE_DIR}..."
if [[ -n "$SANDBOX_ROOTFS_TAR" ]]; then
	if [[ ! -f "$SANDBOX_ROOTFS_TAR" ]]; then
		echo "ERROR: sandbox rootfs tar not found: $SANDBOX_ROOTFS_TAR" >&2
		exit 1
	fi
	unpack_sandbox_rootfs_tar "$SANDBOX_ROOTFS_TAR" "$rootfs_dir"
else
	unpack_sandbox_image "$SANDBOX_IMAGE" "$rootfs_dir"
fi

# Keep non-home tree as root. ensure_sandbox_user restores /home/user to 1000:1000
# afterward — the guest daemon drops to uid 1000 and must be able to write workspace.
maybe_sudo chown -R root:root "$rootfs_dir"
ensure_sandbox_user "$rootfs_dir"
seed_resolv_conf "$rootfs_dir"

used_mb="$(maybe_sudo du -sm "$rootfs_dir" | awk '{print $1}')"
if [[ $((used_mb + ROOTFS_MIN_FREE_MB)) -gt "$FIRECRACKER_ROOTFS_SIZE_MB" ]]; then
	echo "ERROR: staged rootfs is ${used_mb}MiB but FIRECRACKER_ROOTFS_SIZE_MB=${FIRECRACKER_ROOTFS_SIZE_MB} (need ${ROOTFS_MIN_FREE_MB}MiB free after ext4 metadata)" >&2
	echo "Increase FIRECRACKER_ROOTFS_SIZE_MB." >&2
	exit 1
fi
echo "==> Staged rootfs size: ${used_mb}MiB (image ${FIRECRACKER_ROOTFS_SIZE_MB}MiB)"

truncate -s "${FIRECRACKER_ROOTFS_SIZE_MB}M" "$ext4_path"
# -m 0: this image is a single-user sandbox; default 5% reserved blocks are
# invisible to uid 1000 and make a "just fits" tree ENOSPC at guest boot.
maybe_sudo mkfs.ext4 -m 0 -d "$rootfs_dir" -F "$ext4_path" >/dev/null
free_mb="$(ext4_free_mb "$ext4_path")"
if [[ "$free_mb" -lt "$ROOTFS_MIN_FREE_MB" ]]; then
	echo "ERROR: rootfs.ext4 has ${free_mb}MiB free after mkfs (need ${ROOTFS_MIN_FREE_MB}MiB); staged ${used_mb}MiB into ${FIRECRACKER_ROOTFS_SIZE_MB}MiB" >&2
	echo "Increase FIRECRACKER_ROOTFS_SIZE_MB." >&2
	exit 1
fi
echo "==> rootfs.ext4 free space: ${free_mb}MiB"
verify_rootfs_resolv_conf "$ext4_path"

maybe_sudo rm -rf "$TEMPLATE_DIR"
maybe_sudo install -d -m 0755 "$TEMPLATE_DIR"
maybe_sudo install -m 0644 "$FIRECRACKER_CI_VMLINUX" "${TEMPLATE_DIR}/vmlinux"
maybe_sudo install -m 0664 "$ext4_path" "${TEMPLATE_DIR}/rootfs.ext4"
# Jailer uid/gid 1000 must be able to open rootfs RW for snapshot create and restores.
maybe_sudo chown -R 1000:1000 "$TEMPLATE_DIR"

echo "==> Firecracker rootfs template ready: ${TEMPLATE_DIR}"
