#!/usr/bin/env bash
# Packages the Firecracker golden-build scripts and manifest for GitHub Release assets.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION=""
OUTPUT=""

usage() {
	cat >&2 <<EOF
Usage: $0 --version VERSION [--output PATH]

Packages firecracker-golden-build scripts into a tarball for GitHub Release assets.
Default output: dist/firecracker-golden-build-VERSION.tar.gz
EOF
}

while [[ $# -gt 0 ]]; do
	case "$1" in
	--version)
		VERSION="$2"
		shift 2
		;;
	--output)
		OUTPUT="$2"
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

if [[ -z "$VERSION" ]]; then
	echo "--version is required" >&2
	usage
	exit 1
fi

if [[ -z "$OUTPUT" ]]; then
	OUTPUT="${ROOT}/dist/firecracker-golden-build-${VERSION}.tar.gz"
fi

SERVICE_VERSION="$(tr -d '[:space:]' <"${ROOT}/SERVICE_VERSION")"
GIT_SHA="$(git -C "$ROOT" rev-parse HEAD)"
GIT_SHA_SHORT="$(git -C "$ROOT" rev-parse --short HEAD)"
GIT_REF="$(git -C "$ROOT" rev-parse --abbrev-ref HEAD 2>/dev/null || true)"
if [[ "$GIT_REF" == "HEAD" ]]; then
	GIT_REF="$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo "$GIT_SHA_SHORT")"
fi

FIRECRACKER_VERSION="${FIRECRACKER_VERSION:-v1.14.1}"
GO_VERSION="${GO_VERSION:-1.25.0}"
PACKAGED_AT="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

BUNDLE="${WORKDIR}/firecracker-golden-build"
mkdir -p "${BUNDLE}/scripts" "${BUNDLE}/bin"

cp "${ROOT}/scripts/firecracker-golden-build/README.md" "${BUNDLE}/README.md"
install -m 0755 "${ROOT}/e2e/infra/scripts/create-golden-snapshot.sh" "${BUNDLE}/scripts/"
install -m 0755 "${ROOT}/e2e/infra/scripts/build-rootfs-template.sh" "${BUNDLE}/scripts/"
install -m 0755 "${ROOT}/e2e/infra/scripts/configure-host-nat.sh" "${BUNDLE}/scripts/"
install -m 0755 "${ROOT}/e2e/infra/scripts/install-runner-host.sh" "${BUNDLE}/scripts/"
install -m 0755 "${ROOT}/e2e/infra/scripts/firecracker-ci-assets.sh" "${BUNDLE}/scripts/"
install -m 0755 "${ROOT}/e2e/infra/scripts/setup-firecracker-e2e-vm.sh" "${BUNDLE}/scripts/"

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "${BUNDLE}/bin/sandbox-daemon" "${ROOT}/cmd/daemon"
chmod 0755 "${BUNDLE}/bin/sandbox-daemon"
DAEMON_SHA256="$(sha256sum "${BUNDLE}/bin/sandbox-daemon" | awk '{print $1}')"

cat >"${BUNDLE}/MANIFEST.json" <<EOF
{
  "schema_version": 2,
  "service_version": "${SERVICE_VERSION}",
  "bundle_version": "${VERSION}",
  "git_sha": "${GIT_SHA}",
  "git_sha_short": "${GIT_SHA_SHORT}",
  "git_ref": "${GIT_REF}",
  "packaged_at": "${PACKAGED_AT}",
  "firecracker_version": "${FIRECRACKER_VERSION}",
  "go_version": "${GO_VERSION}",
  "entrypoints": {
    "install_runner_host": "scripts/install-runner-host.sh",
    "firecracker_ci_assets": "scripts/firecracker-ci-assets.sh",
    "build_rootfs_template": "scripts/build-rootfs-template.sh",
    "create_snapshot": "scripts/create-golden-snapshot.sh",
    "configure_host_nat": "scripts/configure-host-nat.sh"
  },
  "binaries": {
    "sandbox-daemon": {
      "path": "bin/sandbox-daemon",
      "sha256": "${DAEMON_SHA256}"
    }
  },
  "assets": [
    "README.md",
    "MANIFEST.json",
    "scripts/install-runner-host.sh",
    "scripts/firecracker-ci-assets.sh",
    "scripts/build-rootfs-template.sh",
    "scripts/configure-host-nat.sh",
    "scripts/create-golden-snapshot.sh",
    "scripts/setup-firecracker-e2e-vm.sh",
    "bin/sandbox-daemon"
  ]
}
EOF

mkdir -p "$(dirname "$OUTPUT")"
tar -C "$WORKDIR" -czf "$OUTPUT" firecracker-golden-build

echo "Wrote ${OUTPUT}"
