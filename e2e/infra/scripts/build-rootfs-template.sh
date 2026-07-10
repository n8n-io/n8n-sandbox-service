#!/usr/bin/env bash
# Builds the Firecracker rootfs template (rootfs.ext4 + vmlinux) from Firecracker
# CI assets. Used by e2e VM setup and golden-build bundle consumers (infra runners).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FIRECRACKER_CI_ASSETS_BIN="${FIRECRACKER_CI_ASSETS_BIN:-${SCRIPT_DIR}/firecracker-ci-assets.sh}"

FIRECRACKER_CI_VERSION="${FIRECRACKER_CI_VERSION:-v1.14}"
FIRECRACKER_CI_VMLINUX="${FIRECRACKER_CI_VMLINUX:-}"
FIRECRACKER_CI_ROOTFS_SQUASHFS="${FIRECRACKER_CI_ROOTFS_SQUASHFS:-}"
TEMPLATE_DIR="${TEMPLATE_DIR:-/srv/firecracker/template}"
FIRECRACKER_ROOTFS_SIZE_MB="${FIRECRACKER_ROOTFS_SIZE_MB:-${FIRECRACKER_E2E_ROOTFS_SIZE_MB:-1024}}"

usage() {
	cat >&2 <<EOF
Usage: $0 [options]

Builds rootfs.ext4 and installs vmlinux into TEMPLATE_DIR from Firecracker CI assets.

Options:
  --kernel PATH       Kernel image (FIRECRACKER_CI_VMLINUX)
  --squashfs PATH     Ubuntu squashfs (FIRECRACKER_CI_ROOTFS_SQUASHFS)
  --template-dir DIR  Output directory (TEMPLATE_DIR)
  -h, --help          Show this help

When --kernel / --squashfs are omitted, downloads the latest matching assets from
the public Firecracker CI S3 bucket using FIRECRACKER_CI_VERSION.
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

while [[ $# -gt 0 ]]; do
	case "$1" in
	--kernel)
		FIRECRACKER_CI_VMLINUX="$2"
		shift 2
		;;
	--squashfs)
		FIRECRACKER_CI_ROOTFS_SQUASHFS="$2"
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

work="$(mktemp -d)"
rootfs_dir="${work}/rootfs"
ext4_path="${work}/rootfs.ext4"
cleanup() {
	maybe_sudo rm -rf "$work"
}
trap cleanup EXIT

if [[ -z "$FIRECRACKER_CI_VMLINUX" || -z "$FIRECRACKER_CI_ROOTFS_SQUASHFS" ]]; then
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
if [[ ! -f "$FIRECRACKER_CI_ROOTFS_SQUASHFS" ]]; then
	echo "ERROR: squashfs not found: $FIRECRACKER_CI_ROOTFS_SQUASHFS" >&2
	exit 1
fi

require_elf_kernel "$FIRECRACKER_CI_VMLINUX"

echo "==> Building Firecracker rootfs template in ${TEMPLATE_DIR}..."
unsquashfs -f -d "$rootfs_dir" "$FIRECRACKER_CI_ROOTFS_SQUASHFS" >/dev/null
maybe_sudo chown -R root:root "$rootfs_dir"
if ! maybe_sudo grep -q '^user:' "$rootfs_dir/etc/group"; then
	echo 'user:x:1000:' | maybe_sudo tee -a "$rootfs_dir/etc/group" >/dev/null
fi
if ! maybe_sudo grep -q '^user:' "$rootfs_dir/etc/passwd"; then
	echo 'user:x:1000:1000:Sandbox User:/home/user:/bin/sh' | maybe_sudo tee -a "$rootfs_dir/etc/passwd" >/dev/null
fi
maybe_sudo install -d -m 0755 -o 1000 -g 1000 "$rootfs_dir/home/user"
maybe_sudo install -d -m 1777 -o root -g root "$rootfs_dir/tmp"
# Ubuntu squashfs ships resolv.conf as a symlink; tee follows it and writes
# outside the staged rootfs unless we replace it with a regular file first.
maybe_sudo rm -f "$rootfs_dir/etc/resolv.conf"
printf 'nameserver 8.8.8.8\nnameserver 1.1.1.1\n' | maybe_sudo tee "$rootfs_dir/etc/resolv.conf" >/dev/null
truncate -s "${FIRECRACKER_ROOTFS_SIZE_MB}M" "$ext4_path"
maybe_sudo mkfs.ext4 -d "$rootfs_dir" -F "$ext4_path" >/dev/null
verify_rootfs_resolv_conf "$ext4_path"

maybe_sudo rm -rf "$TEMPLATE_DIR"
maybe_sudo install -d -m 0755 "$TEMPLATE_DIR"
maybe_sudo install -m 0644 "$FIRECRACKER_CI_VMLINUX" "${TEMPLATE_DIR}/vmlinux"
maybe_sudo install -m 0664 "$ext4_path" "${TEMPLATE_DIR}/rootfs.ext4"
maybe_sudo chown -R "$(id -u):$(id -g)" "$TEMPLATE_DIR"
maybe_sudo chmod 0664 "${TEMPLATE_DIR}/rootfs.ext4"
maybe_sudo chmod 0644 "${TEMPLATE_DIR}/vmlinux"

echo "==> Firecracker rootfs template ready: ${TEMPLATE_DIR}"
