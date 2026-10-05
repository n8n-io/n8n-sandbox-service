#!/usr/bin/env bash
# The Firecracker release pin. Dockerfile.ee.runner-firecracker, the host
# installer (install-runner-host.sh), the e2e VM setup and the golden-build
# bundle all take the version and checksum from here: the host binary creates
# the golden snapshot, the runner-image binary restores it, and Firecracker
# cannot restore a snapshot taken by a newer version.
set -euo pipefail

FIRECRACKER_RELEASE_BASE_URL=https://github.com/firecracker-microvm/firecracker/releases/download
FIRECRACKER_RELEASE_ARCH=x86_64
FIRECRACKER_RELEASE_INSTALL_DIR="${FIRECRACKER_RELEASE_INSTALL_DIR:-/opt/firecracker/bin}"
# The pin. The SHA-256 is upstream's, published next to the tarball as
# ${FIRECRACKER_RELEASE_BASE_URL}/<version>/firecracker-<version>-x86_64.tgz.sha256.txt.
# bump-firecracker.sh rewrites these two lines.
FIRECRACKER_DEFAULT_VERSION=v1.14.4
FIRECRACKER_DEFAULT_X86_64_SHA256=ca8ffc4ae778cdbd5de8a598a246d611542cf2bdbc3b22879eab928b653a5d1a
FIRECRACKER_VERSION="${FIRECRACKER_VERSION:-$FIRECRACKER_DEFAULT_VERSION}"
FIRECRACKER_VERSION="v${FIRECRACKER_VERSION#v}"
FIRECRACKER_TARBALL_SHA256="${FIRECRACKER_TARBALL_SHA256:-}"

firecracker_release_usage() {
	cat <<'EOF'
Usage:
  firecracker-release.sh install [DEST_DIR]

Download the pinned Firecracker release tarball, verify its SHA-256 and install
firecracker and jailer into DEST_DIR (default: /opt/firecracker/bin).

Environment:
  FIRECRACKER_VERSION         Release tag (default: pinned in this script)
  FIRECRACKER_TARBALL_SHA256  Required when FIRECRACKER_VERSION is not the pinned default
EOF
}

firecracker_release_tarball_name() {
	echo "firecracker-${FIRECRACKER_VERSION}-${FIRECRACKER_RELEASE_ARCH}.tgz"
}

firecracker_release_tarball_url() {
	echo "${FIRECRACKER_RELEASE_BASE_URL}/${FIRECRACKER_VERSION}/$(firecracker_release_tarball_name)"
}

firecracker_release_resolve_sha256() {
	if [[ -n "$FIRECRACKER_TARBALL_SHA256" ]]; then
		return 0
	fi
	if [[ "$FIRECRACKER_VERSION" != "$FIRECRACKER_DEFAULT_VERSION" ]]; then
		echo "ERROR: FIRECRACKER_TARBALL_SHA256 is required for ${FIRECRACKER_VERSION}; the pinned SHA-256 covers ${FIRECRACKER_DEFAULT_VERSION} only" >&2
		return 1
	fi
	FIRECRACKER_TARBALL_SHA256="$FIRECRACKER_DEFAULT_X86_64_SHA256"
}

# Subshell body: the EXIT trap below stays private to this call (a RETURN trap
# set in a function leaks into its callers) and runs when set -e aborts it, so
# the scripts that source this file keep their own EXIT trap and no temp dir
# is left behind.
firecracker_release_install() (
	local dest_dir=${1:-$FIRECRACKER_RELEASE_INSTALL_DIR}
	local work url release_dir

	if [[ "$(uname -m)" != "$FIRECRACKER_RELEASE_ARCH" ]]; then
		echo "ERROR: Firecracker release install supports ${FIRECRACKER_RELEASE_ARCH} only; got $(uname -m)" >&2
		return 1
	fi
	firecracker_release_resolve_sha256

	work="$(mktemp -d)"
	trap 'rm -rf "$work"' EXIT
	url="$(firecracker_release_tarball_url)"
	release_dir="${work}/release-${FIRECRACKER_VERSION}-${FIRECRACKER_RELEASE_ARCH}"

	echo "==> Installing Firecracker ${FIRECRACKER_VERSION} into ${dest_dir}..."
	if ! curl -fsSL "$url" -o "${work}/firecracker.tgz"; then
		echo "ERROR: failed to download ${url}" >&2
		return 1
	fi
	if ! echo "${FIRECRACKER_TARBALL_SHA256}  ${work}/firecracker.tgz" | sha256sum -c - >/dev/null; then
		echo "ERROR: Firecracker tarball SHA-256 mismatch: ${url} (expected ${FIRECRACKER_TARBALL_SHA256})" >&2
		return 1
	fi
	tar -xzf "${work}/firecracker.tgz" -C "$work"
	install -d -m 0755 "$dest_dir"
	install -m 0755 \
		"${release_dir}/firecracker-${FIRECRACKER_VERSION}-${FIRECRACKER_RELEASE_ARCH}" \
		"${dest_dir}/firecracker"
	install -m 0755 \
		"${release_dir}/jailer-${FIRECRACKER_VERSION}-${FIRECRACKER_RELEASE_ARCH}" \
		"${dest_dir}/jailer"
)

firecracker_release_main() {
	case "${1:-}" in
	install)
		firecracker_release_install "${2:-$FIRECRACKER_RELEASE_INSTALL_DIR}"
		;;
	-h | --help | help)
		firecracker_release_usage
		;;
	"")
		firecracker_release_usage >&2
		return 1
		;;
	*)
		echo "ERROR: unknown command: $1" >&2
		firecracker_release_usage >&2
		return 1
		;;
	esac
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
	firecracker_release_main "$@"
fi
