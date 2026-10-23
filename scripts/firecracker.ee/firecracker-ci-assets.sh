#!/usr/bin/env bash
# Download Firecracker CI kernel (vmlinux) from the public spec.ccfc.min bucket.
# Guest userspace comes from the sandbox OCI image (see build-rootfs-template.sh).
set -euo pipefail

FIRECRACKER_CI_S3_BASE="${FIRECRACKER_CI_S3_BASE:-https://s3.amazonaws.com/spec.ccfc.min}"
FIRECRACKER_CI_ASSETS_DIR="${FIRECRACKER_CI_ASSETS_DIR:-/srv/firecracker/ci-assets}"
# The pin. Firecracker publishes guest kernels only in its weekly CI builds
# (firecracker-ci/<date>-<commit>-<n>/), not per release, so the path names the
# build this kernel was taken from. The bucket publishes no checksums, so the
# SHA-256 was taken from the artifact when it was adopted; it freezes that
# artifact rather than proving its origin. bump-firecracker-kernel.sh rewrites
# these three lines.
FIRECRACKER_CI_DEFAULT_KERNEL_VERSION=6.1.188
FIRECRACKER_CI_DEFAULT_VMLINUX_SHA256=c16288854c798ded46c45b3ccab4bc85b8041552ab50f899fecbad550a641e92
FIRECRACKER_CI_DEFAULT_VMLINUX_PATH=firecracker-ci/20261016-5732b8331e1e-0/x86_64/vmlinux-6.1.188

firecracker_ci_assets_usage() {
	cat <<'EOF'
Usage:
  firecracker-ci-assets.sh download [DEST_DIR]
  firecracker-ci-assets.sh verify [DEST_DIR]

Download the pinned Firecracker CI vmlinux into DEST_DIR, verify its SHA-256 and
write manifest.env for rootfs template / snapshot builds. To use another
kernel, pass it to build-rootfs-template.sh --kernel.
EOF
}

firecracker_ci_assets_manifest_path() {
	local dest_dir=${1:-$FIRECRACKER_CI_ASSETS_DIR}
	echo "${dest_dir}/manifest.env"
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
	local manifest

	if [[ "$(uname -m)" != "x86_64" ]]; then
		echo "ERROR: Firecracker CI assets support amd64/x86_64 only" >&2
		return 1
	fi
	# The manifest records the version, so a path that names another kernel would
	# mislabel the one downloaded.
	if [[ "$FIRECRACKER_CI_DEFAULT_VMLINUX_PATH" != */x86_64/vmlinux-"$FIRECRACKER_CI_DEFAULT_KERNEL_VERSION" ]]; then
		echo "ERROR: pinned path ${FIRECRACKER_CI_DEFAULT_VMLINUX_PATH} is not x86_64 vmlinux-${FIRECRACKER_CI_DEFAULT_KERNEL_VERSION}" >&2
		return 1
	fi

	install -d -m 0755 "$dest_dir"

	echo "==> Downloading Firecracker CI vmlinux-${FIRECRACKER_CI_DEFAULT_KERNEL_VERSION} into ${dest_dir}..."
	curl -fsSL "${FIRECRACKER_CI_S3_BASE}/${FIRECRACKER_CI_DEFAULT_VMLINUX_PATH}" -o "${dest_dir}/vmlinux"
	firecracker_ci_assets_require_sha256 "${dest_dir}/vmlinux" "$FIRECRACKER_CI_DEFAULT_VMLINUX_SHA256"
	firecracker_ci_assets_require_elf_kernel "${dest_dir}/vmlinux"
	chmod 0644 "${dest_dir}/vmlinux"

	manifest="$(firecracker_ci_assets_manifest_path "$dest_dir")"
	cat >"$manifest" <<EOF
FIRECRACKER_CI_KERNEL_VERSION=${FIRECRACKER_CI_DEFAULT_KERNEL_VERSION}
FIRECRACKER_CI_VMLINUX=${dest_dir}/vmlinux
FIRECRACKER_CI_VMLINUX_SHA256=${FIRECRACKER_CI_DEFAULT_VMLINUX_SHA256}
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
