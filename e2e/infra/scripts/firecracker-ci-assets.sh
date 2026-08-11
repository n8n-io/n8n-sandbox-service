#!/usr/bin/env bash
# Download Firecracker CI kernel (vmlinux) from the public spec.ccfc.min bucket.
# Guest userspace comes from the sandbox OCI image (see build-rootfs-template.sh).
set -euo pipefail

FIRECRACKER_CI_S3_BASE="${FIRECRACKER_CI_S3_BASE:-https://s3.amazonaws.com/spec.ccfc.min}"
FIRECRACKER_CI_ASSETS_DIR="${FIRECRACKER_CI_ASSETS_DIR:-/srv/firecracker/ci-assets}"
FIRECRACKER_CI_VERSION="${FIRECRACKER_CI_VERSION:-v1.14}"

firecracker_ci_assets_usage() {
	cat <<'EOF'
Usage:
  firecracker-ci-assets.sh download [DEST_DIR]
  firecracker-ci-assets.sh verify [DEST_DIR]

Download Firecracker CI vmlinux into DEST_DIR and write manifest.env for rootfs
template / snapshot builds. Ubuntu squashfs is no longer used for guest userspace.
EOF
}

firecracker_ci_assets_manifest_path() {
	local dest_dir=${1:-$FIRECRACKER_CI_ASSETS_DIR}
	echo "${dest_dir}/manifest.env"
}

firecracker_ci_assets_s3_latest_key() {
	local prefix=$1 pattern=$2 key
	key="$(
		curl -fsSL "${FIRECRACKER_CI_S3_BASE}/?prefix=${prefix}&list-type=2" |
			tr '<' '\n' |
			sed -n 's#^Key>\(firecracker-ci/[^<]*\)#\1#p' |
			grep -E "$pattern" |
			sort -V |
			tail -n 1
	)"
	if [[ -z "$key" ]]; then
		echo "ERROR: could not find Firecracker CI asset for prefix ${prefix}" >&2
		return 1
	fi
	echo "$key"
}

firecracker_ci_assets_require_elf_kernel() {
	local path=$1
	if ! LC_ALL=C od -An -N4 -tx1 "$path" | tr -d ' \n' | grep -qi '^7f454c46$'; then
		echo "ERROR: kernel must be an uncompressed ELF vmlinux image: $path" >&2
		return 1
	fi
}

firecracker_ci_assets_download() {
	local dest_dir=${1:-$FIRECRACKER_CI_ASSETS_DIR}
	local arch ci_version kernel_key manifest

	if [[ "$(uname -m)" != "x86_64" ]]; then
		echo "ERROR: Firecracker CI assets support amd64/x86_64 only" >&2
		return 1
	fi

	arch="$(uname -m)"
	ci_version="${FIRECRACKER_CI_VERSION#v}"
	ci_version="v${ci_version}"

	install -d -m 0755 "$dest_dir"

	kernel_key="$(firecracker_ci_assets_s3_latest_key \
		"firecracker-ci/${ci_version}/${arch}/vmlinux-" \
		"firecracker-ci/${ci_version}/${arch}/vmlinux-[0-9]+\\.[0-9]+\\.[0-9]+$")"

	echo "==> Downloading Firecracker CI ${ci_version} vmlinux into ${dest_dir}..."
	curl -fsSL "${FIRECRACKER_CI_S3_BASE}/${kernel_key}" -o "${dest_dir}/vmlinux"
	firecracker_ci_assets_require_elf_kernel "${dest_dir}/vmlinux"
	chmod 0644 "${dest_dir}/vmlinux"

	manifest="$(firecracker_ci_assets_manifest_path "$dest_dir")"
	cat >"$manifest" <<EOF
FIRECRACKER_CI_VERSION=${ci_version}
FIRECRACKER_CI_VMLINUX=${dest_dir}/vmlinux
EOF
	chmod 0644 "$manifest"
	echo "==> Wrote ${manifest}"
}

firecracker_ci_assets_verify() {
	local dest_dir=${1:-$FIRECRACKER_CI_ASSETS_DIR}
	local manifest vmlinux

	manifest="$(firecracker_ci_assets_manifest_path "$dest_dir")"
	if [[ ! -f "$manifest" ]]; then
		echo "ERROR: missing Firecracker CI manifest: $manifest" >&2
		return 1
	fi

	# shellcheck disable=SC1090
	source "$manifest"

	vmlinux="${FIRECRACKER_CI_VMLINUX:-}"
	if [[ -z "$vmlinux" ]]; then
		echo "ERROR: manifest is missing FIRECRACKER_CI_VMLINUX: $manifest" >&2
		return 1
	fi
	if [[ ! -f "$vmlinux" ]]; then
		echo "ERROR: Firecracker CI kernel missing under ${dest_dir}" >&2
		return 1
	fi

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
