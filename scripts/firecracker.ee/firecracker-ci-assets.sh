#!/usr/bin/env bash
# Download Firecracker CI kernel (vmlinux) from the public spec.ccfc.min bucket.
# Guest userspace comes from the sandbox OCI image (see build-rootfs-template.sh).
set -euo pipefail

FIRECRACKER_CI_S3_BASE="${FIRECRACKER_CI_S3_BASE:-https://s3.amazonaws.com/spec.ccfc.min}"
FIRECRACKER_CI_ASSETS_DIR="${FIRECRACKER_CI_ASSETS_DIR:-/srv/firecracker/ci-assets}"
# The pin: SHA-256 of firecracker-ci/<CI version>/x86_64/vmlinux-<kernel version>.
# The bucket publishes no checksums, so the SHA-256 was taken from the artifact
# when it was adopted; it freezes that artifact rather than proving its origin.
# bump-firecracker-kernel.sh rewrites the kernel version and SHA-256 lines.
FIRECRACKER_CI_DEFAULT_VERSION=v1.14
FIRECRACKER_CI_DEFAULT_KERNEL_VERSION=6.1.155
FIRECRACKER_CI_DEFAULT_VMLINUX_SHA256=e41c7048bd2475e7e788153823fcb9166a7e0b78c4c443bd6446d015fa735f53
FIRECRACKER_CI_VERSION="${FIRECRACKER_CI_VERSION:-$FIRECRACKER_CI_DEFAULT_VERSION}"
FIRECRACKER_CI_VERSION="v${FIRECRACKER_CI_VERSION#v}"
FIRECRACKER_CI_KERNEL_VERSION="${FIRECRACKER_CI_KERNEL_VERSION:-$FIRECRACKER_CI_DEFAULT_KERNEL_VERSION}"
FIRECRACKER_CI_VMLINUX_SHA256="${FIRECRACKER_CI_VMLINUX_SHA256:-}"

firecracker_ci_assets_usage() {
	cat <<'EOF'
Usage:
  firecracker-ci-assets.sh download [DEST_DIR]
  firecracker-ci-assets.sh verify [DEST_DIR]

Download the pinned Firecracker CI vmlinux into DEST_DIR, verify its SHA-256 and
write manifest.env for rootfs template / snapshot builds.

Environment:
  FIRECRACKER_CI_VERSION         Bucket version (default: pinned in this script)
  FIRECRACKER_CI_KERNEL_VERSION  Kernel version (default: pinned in this script)
  FIRECRACKER_CI_VMLINUX_SHA256  Required when either differs from the pinned default
EOF
}

firecracker_ci_assets_manifest_path() {
	local dest_dir=${1:-$FIRECRACKER_CI_ASSETS_DIR}
	echo "${dest_dir}/manifest.env"
}

firecracker_ci_assets_resolve_vmlinux_sha256() {
	if [[ -n "$FIRECRACKER_CI_VMLINUX_SHA256" ]]; then
		return 0
	fi
	if [[ "$FIRECRACKER_CI_VERSION" != "$FIRECRACKER_CI_DEFAULT_VERSION" ||
		"$FIRECRACKER_CI_KERNEL_VERSION" != "$FIRECRACKER_CI_DEFAULT_KERNEL_VERSION" ]]; then
		echo "ERROR: FIRECRACKER_CI_VMLINUX_SHA256 is required for ${FIRECRACKER_CI_VERSION} vmlinux-${FIRECRACKER_CI_KERNEL_VERSION}; the pinned SHA-256 covers ${FIRECRACKER_CI_DEFAULT_VERSION} vmlinux-${FIRECRACKER_CI_DEFAULT_KERNEL_VERSION} only" >&2
		return 1
	fi
	FIRECRACKER_CI_VMLINUX_SHA256="$FIRECRACKER_CI_DEFAULT_VMLINUX_SHA256"
}

firecracker_ci_assets_require_elf_kernel() {
	local path=$1
	if ! LC_ALL=C od -An -N4 -tx1 "$path" | tr -d ' \n' | grep -qi '^7f454c46$'; then
		echo "ERROR: kernel must be an uncompressed ELF vmlinux image: $path" >&2
		return 1
	fi
}

firecracker_ci_assets_require_sha256() {
	local path=$1 sha256=$2
	if ! echo "${sha256}  ${path}" | sha256sum -c - >/dev/null; then
		echo "ERROR: kernel SHA-256 mismatch: $path (expected ${sha256})" >&2
		return 1
	fi
}

firecracker_ci_assets_download() {
	local dest_dir=${1:-$FIRECRACKER_CI_ASSETS_DIR}
	local arch kernel_key manifest

	if [[ "$(uname -m)" != "x86_64" ]]; then
		echo "ERROR: Firecracker CI assets support amd64/x86_64 only" >&2
		return 1
	fi

	arch="$(uname -m)"
	firecracker_ci_assets_resolve_vmlinux_sha256

	install -d -m 0755 "$dest_dir"

	kernel_key="firecracker-ci/${FIRECRACKER_CI_VERSION}/${arch}/vmlinux-${FIRECRACKER_CI_KERNEL_VERSION}"

	echo "==> Downloading Firecracker CI ${FIRECRACKER_CI_VERSION} vmlinux-${FIRECRACKER_CI_KERNEL_VERSION} into ${dest_dir}..."
	curl -fsSL "${FIRECRACKER_CI_S3_BASE}/${kernel_key}" -o "${dest_dir}/vmlinux"
	firecracker_ci_assets_require_sha256 "${dest_dir}/vmlinux" "$FIRECRACKER_CI_VMLINUX_SHA256"
	firecracker_ci_assets_require_elf_kernel "${dest_dir}/vmlinux"
	chmod 0644 "${dest_dir}/vmlinux"

	manifest="$(firecracker_ci_assets_manifest_path "$dest_dir")"
	cat >"$manifest" <<EOF
FIRECRACKER_CI_VERSION=${FIRECRACKER_CI_VERSION}
FIRECRACKER_CI_KERNEL_VERSION=${FIRECRACKER_CI_KERNEL_VERSION}
FIRECRACKER_CI_VMLINUX=${dest_dir}/vmlinux
FIRECRACKER_CI_VMLINUX_SHA256=${FIRECRACKER_CI_VMLINUX_SHA256}
EOF
	chmod 0644 "$manifest"
	echo "==> Wrote ${manifest}"
}

firecracker_ci_assets_verify() {
	local dest_dir=${1:-$FIRECRACKER_CI_ASSETS_DIR}
	local manifest vmlinux sha256

	manifest="$(firecracker_ci_assets_manifest_path "$dest_dir")"
	if [[ ! -f "$manifest" ]]; then
		echo "ERROR: missing Firecracker CI manifest: $manifest" >&2
		return 1
	fi

	# shellcheck disable=SC1090
	source "$manifest"

	vmlinux="${FIRECRACKER_CI_VMLINUX:-}"
	sha256="${FIRECRACKER_CI_VMLINUX_SHA256:-}"
	if [[ -z "$vmlinux" || -z "$sha256" ]]; then
		echo "ERROR: manifest is missing FIRECRACKER_CI_VMLINUX or FIRECRACKER_CI_VMLINUX_SHA256 (re-run download): $manifest" >&2
		return 1
	fi
	if [[ ! -f "$vmlinux" ]]; then
		echo "ERROR: Firecracker CI kernel missing under ${dest_dir}" >&2
		return 1
	fi

	firecracker_ci_assets_require_sha256 "$vmlinux" "$sha256"
	firecracker_ci_assets_require_elf_kernel "$vmlinux"
	echo "==> Firecracker CI assets are ready in ${dest_dir}"
}

firecracker_ci_assets_main() {
	case "${1:-}" in
	download)
		firecracker_ci_assets_download "${2:-$FIRECRACKER_CI_ASSETS_DIR}"
		;;
	verify)
		firecracker_ci_assets_verify "${2:-$FIRECRACKER_CI_ASSETS_DIR}"
		;;
	-h | --help | help)
		firecracker_ci_assets_usage
		;;
	"")
		firecracker_ci_assets_usage >&2
		return 1
		;;
	*)
		echo "ERROR: unknown command: $1" >&2
		firecracker_ci_assets_usage >&2
		return 1
		;;
	esac
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
	firecracker_ci_assets_main "$@"
fi
