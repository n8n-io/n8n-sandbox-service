#!/usr/bin/env bash
# CI self-test for the Firecracker golden-build bundle v2 scripts.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUILD_SCRIPT="${ROOT}/e2e/infra/scripts/build-rootfs-template.sh"
FIRECRACKER_CI_VERSION="${FIRECRACKER_CI_VERSION:-v1.14}"

if [[ "$(uname -m)" != "x86_64" ]]; then
	echo "golden-build bundle self-test requires linux/amd64; got $(uname -m)" >&2
	exit 1
fi

for cmd in curl unsquashfs mkfs.ext4 truncate debugfs; do
	if ! command -v "$cmd" >/dev/null 2>&1; then
		echo "missing required command: $cmd" >&2
		exit 1
	fi
done

work="$(mktemp -d)"
template_dir="${work}/template"
trap 'rm -rf "$work"' EXIT

echo "==> Running build-rootfs-template.sh (FIRECRACKER_CI_VERSION=${FIRECRACKER_CI_VERSION})..."
FIRECRACKER_CI_VERSION="$FIRECRACKER_CI_VERSION" \
	FIRECRACKER_ROOTFS_SIZE_MB=256 \
	TEMPLATE_DIR="$template_dir" \
	bash "$BUILD_SCRIPT"

for path in "${template_dir}/vmlinux" "${template_dir}/rootfs.ext4"; do
	if [[ ! -f "$path" ]]; then
		echo "ERROR: expected output missing: $path" >&2
		exit 1
	fi
done

resolv="$(debugfs -R 'cat /etc/resolv.conf' "${template_dir}/rootfs.ext4" 2>/dev/null || true)"
if ! grep -q 'nameserver 8.8.8.8' <<<"$resolv" || ! grep -q 'nameserver 1.1.1.1' <<<"$resolv"; then
	echo "ERROR: rootfs.ext4 /etc/resolv.conf missing seeded nameservers" >&2
	printf '%s\n' "$resolv" >&2
	exit 1
fi

echo "==> Packaging golden-build bundle..."
bundle_tar="${work}/bundle.tar.gz"
bash "${ROOT}/scripts/package-firecracker-golden-build.sh" \
	--version "ci-self-test" \
	--output "$bundle_tar"

bundle_root="${work}/unpacked"
mkdir -p "$bundle_root"
tar -C "$bundle_root" -xzf "$bundle_tar"

manifest="${bundle_root}/firecracker-golden-build/MANIFEST.json"
for required in \
	"scripts/build-rootfs-template.sh" \
	"scripts/configure-host-nat.sh" \
	"scripts/create-golden-snapshot.sh" \
	"bin/sandbox-daemon"; do
	if [[ ! -f "${bundle_root}/firecracker-golden-build/${required}" ]]; then
		echo "ERROR: bundle missing ${required}" >&2
		exit 1
	fi
done

schema_version="$(node -e 'const m=require(process.argv[1]); process.stdout.write(String(m.schema_version))' "$manifest")"
if [[ "$schema_version" != "2" ]]; then
	echo "ERROR: expected MANIFEST.json schema_version 2, got ${schema_version}" >&2
	exit 1
fi

echo "OK    golden-build bundle v2 self-test passed"
