#!/usr/bin/env bash
# CI self-test for the Firecracker golden-build bundle v2 scripts.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUILD_SCRIPT="${ROOT}/scripts/firecracker.ee/build-rootfs-template.sh"
FIRECRACKER_CI_VERSION="${FIRECRACKER_CI_VERSION:-v1.14}"
SANDBOX_IMAGE="${SANDBOX_IMAGE:-n8n-sandbox:golden-build-self-test}"
FIRECRACKER_ROOTFS_SIZE_MB="${FIRECRACKER_ROOTFS_SIZE_MB:-2048}"

if [[ "$(uname -m)" != "x86_64" ]]; then
	echo "golden-build bundle self-test requires linux/amd64; got $(uname -m)" >&2
	exit 1
fi

for cmd in curl docker mkfs.ext4 truncate debugfs jq; do
	if ! command -v "$cmd" >/dev/null 2>&1; then
		echo "missing required command: $cmd" >&2
		exit 1
	fi
done

maybe_sudo() {
	if [[ "$(id -u)" -eq 0 ]]; then
		"$@"
	else
		sudo "$@"
	fi
}

work="$(mktemp -d)"
template_dir="${work}/template"
# build-rootfs-template.sh chowns TEMPLATE_DIR to 1000:1000 for the jailer,
# so cleanup must escalate when the CI runner is a different uid.
trap 'maybe_sudo rm -rf "$work"' EXIT

echo "==> Building sandbox image ${SANDBOX_IMAGE} from Dockerfile.sandbox..."
docker build -f "${ROOT}/Dockerfile.sandbox" -t "$SANDBOX_IMAGE" "$ROOT"

echo "==> Running build-rootfs-template.sh (FIRECRACKER_CI_VERSION=${FIRECRACKER_CI_VERSION})..."
FIRECRACKER_CI_VERSION="$FIRECRACKER_CI_VERSION" \
	SANDBOX_IMAGE="$SANDBOX_IMAGE" \
	FIRECRACKER_ROOTFS_SIZE_MB="$FIRECRACKER_ROOTFS_SIZE_MB" \
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

workspace="$(debugfs -R 'stat /home/user/workspace/package.json' "${template_dir}/rootfs.ext4" 2>/dev/null || true)"
if [[ -z "$workspace" ]] || grep -qi 'No such file' <<<"$workspace"; then
	echo "ERROR: rootfs.ext4 missing sandbox workspace package.json" >&2
	printf '%s\n' "$workspace" >&2
	exit 1
fi
# debugfs prints "User:  1000   Group:  1000" (spacing varies).
if ! grep -Eq 'User:[[:space:]]*1000[[:space:]]+Group:[[:space:]]*1000' <<<"$workspace"; then
	echo "ERROR: /home/user/workspace/package.json must be owned by 1000:1000 (daemon drops privileges)" >&2
	printf '%s\n' "$workspace" >&2
	exit 1
fi

echo "==> Packaging golden-build bundle..."
bundle_tar="${work}/bundle.tar.gz"
FIRECRACKER_ROOTFS_SIZE_MB="$FIRECRACKER_ROOTFS_SIZE_MB" \
	bash "${ROOT}/scripts/package-firecracker-golden-build.sh" \
	--version "ci-self-test" \
	--output "$bundle_tar"

bundle_dir="${work}/unpacked/firecracker-golden-build"
mkdir -p "$(dirname "$bundle_dir")"
tar -C "${work}/unpacked" -xzf "$bundle_tar"

manifest="${bundle_dir}/MANIFEST.json"
if [[ "$(jq -r .schema_version "$manifest")" != "2" ]]; then
	echo "ERROR: expected MANIFEST.json schema_version 2" >&2
	exit 1
fi
sandbox_ref="$(jq -r .sandbox_image.ref "$manifest")"
sandbox_repo="$(jq -r .sandbox_image.repository "$manifest")"
sandbox_tag="$(jq -r .sandbox_image.tag "$manifest")"
if [[ "$sandbox_ref" == "null" || -z "$sandbox_ref" ]]; then
	echo "ERROR: MANIFEST.json missing sandbox_image.ref" >&2
	exit 1
fi
if [[ -n "$sandbox_tag" ]]; then
	if [[ "$sandbox_ref" != "${sandbox_repo}:${sandbox_tag}" ]]; then
		echo "ERROR: sandbox_image.ref does not match repository:tag (${sandbox_repo}:${sandbox_tag} vs ${sandbox_ref})" >&2
		exit 1
	fi
elif [[ "$sandbox_ref" != "$sandbox_repo" && "$sandbox_ref" != "${sandbox_repo}@"* ]]; then
	echo "ERROR: sandbox_image.ref does not match repository (${sandbox_repo} vs ${sandbox_ref})" >&2
	exit 1
fi
if [[ "$(jq -r .firecracker_rootfs_size_mb "$manifest")" != "$FIRECRACKER_ROOTFS_SIZE_MB" ]]; then
	echo "ERROR: expected firecracker_rootfs_size_mb ${FIRECRACKER_ROOTFS_SIZE_MB}" >&2
	exit 1
fi

for path in \
	"scripts/install-runner-host.sh" \
	"scripts/firecracker-ci-assets.sh" \
	"scripts/build-rootfs-template.sh" \
	"scripts/configure-host-nat.sh" \
	"scripts/create-golden-snapshot.sh" \
	"bin/sandbox-daemon"; do
	if [[ ! -f "${bundle_dir}/${path}" || ! -x "${bundle_dir}/${path}" ]]; then
		echo "ERROR: bundle missing or non-executable: ${path}" >&2
		ls -l "${bundle_dir}/${path}" >&2 2>/dev/null || true
		exit 1
	fi
done

echo "OK    golden-build bundle v2 self-test passed"
