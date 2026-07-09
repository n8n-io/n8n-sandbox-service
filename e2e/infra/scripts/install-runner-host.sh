#!/usr/bin/env bash
# Installs generic Firecracker runner host prerequisites on Linux amd64.
#
# Out of scope (infra / gallery build supplies these):
#   - ACR image pulls, crane, baked runner/daemon binaries
#   - Golden-build bundle install, systemd units, Key Vault, cloud-init
#
# Intended callers:
#   - Infra gallery image build (optionally bake Firecracker CI assets)
#   - e2e VM setup (setup-firecracker-e2e-vm.sh)
#   - First-boot on a runner after bundle install
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

FIRECRACKER_VERSION="${FIRECRACKER_VERSION:-v1.14.1}"
FIRECRACKER_TARBALL_SHA256="${FIRECRACKER_TARBALL_SHA256:-}"
JAILER_TMPFS_SIZE="${JAILER_TMPFS_SIZE:-8G}"
FIRECRACKER_CI_ASSETS_DIR="${FIRECRACKER_CI_ASSETS_DIR:-/srv/firecracker/ci-assets}"
FIRECRACKER_CI_VERSION="${FIRECRACKER_CI_VERSION:-v1.14}"
DOWNLOAD_FIRECRACKER_CI_ASSETS="${DOWNLOAD_FIRECRACKER_CI_ASSETS:-0}"
CONFIGURE_HOST_NAT_SCRIPT="${CONFIGURE_HOST_NAT_SCRIPT:-${SCRIPT_DIR}/configure-host-nat.sh}"
FIRECRACKER_CI_ASSETS_BIN="${FIRECRACKER_CI_ASSETS_BIN:-${SCRIPT_DIR}/firecracker-ci-assets.sh}"

SKIP_PACKAGES=0
SKIP_FIRECRACKER=0

usage() {
	cat >&2 <<EOF
Usage: $0 [options]

Install host packages, Firecracker binaries, runtime directories, persistent IP
forwarding, and NAT/FORWARD rules for sandbox netns egress.

Options:
  --skip-packages           Skip apt package install
  --skip-firecracker        Skip Firecracker/jailer install
  --download-ci-assets      Download Firecracker CI kernel/squashfs into DEST_DIR
  -h, --help                Show this help

Environment:
  FIRECRACKER_VERSION           Firecracker release tag (default: v1.14.1)
  FIRECRACKER_TARBALL_SHA256    Required when version has no built-in checksum
  JAILER_TMPFS_SIZE             tmpfs size for /srv/jailer (default: 8G)
  FIRECRACKER_CI_ASSETS_DIR     CI asset directory (default: /srv/firecracker/ci-assets)
  FIRECRACKER_CI_VERSION        CI bucket version for --download-ci-assets
  DOWNLOAD_FIRECRACKER_CI_ASSETS  Set to 1 to download CI assets (same as flag)
  CONFIGURE_HOST_NAT_SCRIPT     Path to configure-host-nat.sh
  FIRECRACKER_CI_ASSETS_BIN     Path to firecracker-ci-assets.sh
EOF
}

require_root() {
	if [[ "$(id -u)" -ne 0 ]]; then
		echo "ERROR: $0 must run as root" >&2
		exit 1
	fi
}

resolve_firecracker_tarball_sha256() {
	if [[ -n "$FIRECRACKER_TARBALL_SHA256" ]]; then
		return 0
	fi
	case "$FIRECRACKER_VERSION" in
	v1.13.1)
		FIRECRACKER_TARBALL_SHA256="59450b9171ff2ebdf2f9a25be3a248a7ba79fb6371aec51a9d6d8eefca7b4faf"
		;;
	v1.14.1)
		FIRECRACKER_TARBALL_SHA256="ea66dc1fbdb2473bbb95a1e822ae7884cd575a891a8f801258723258d36b7c7c"
		;;
	*)
		echo "ERROR: FIRECRACKER_TARBALL_SHA256 is required for ${FIRECRACKER_VERSION}" >&2
		exit 1
		;;
	esac
}

install_host_packages() {
	echo "==> Installing host packages..."
	apt-get update -qq
	DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
		binutils \
		ca-certificates \
		curl \
		e2fsprogs \
		file \
		gzip \
		iproute2 \
		iptables \
		jq \
		make \
		openssl \
		squashfs-tools \
		sudo \
		tar \
		util-linux
}

install_firecracker_release() {
	local tmp_fc
	echo "==> Installing Firecracker ${FIRECRACKER_VERSION}..."
	tmp_fc="$(mktemp -d)"
	trap 'rm -rf "$tmp_fc"' RETURN
	curl -fsSL \
		"https://github.com/firecracker-microvm/firecracker/releases/download/${FIRECRACKER_VERSION}/firecracker-${FIRECRACKER_VERSION}-x86_64.tgz" \
		-o "$tmp_fc/firecracker.tgz"
	echo "${FIRECRACKER_TARBALL_SHA256}  $tmp_fc/firecracker.tgz" | sha256sum -c -
	tar -xzf "$tmp_fc/firecracker.tgz" -C "$tmp_fc"
	install -m 0755 -d /opt/firecracker/bin
	install -m 0755 \
		"$tmp_fc/release-${FIRECRACKER_VERSION}-x86_64/firecracker-${FIRECRACKER_VERSION}-x86_64" \
		/opt/firecracker/bin/firecracker
	install -m 0755 \
		"$tmp_fc/release-${FIRECRACKER_VERSION}-x86_64/jailer-${FIRECRACKER_VERSION}-x86_64" \
		/opt/firecracker/bin/jailer
}

prepare_firecracker_dirs() {
	echo "==> Preparing Firecracker directories..."
	install -d -m 0755 \
		/opt/n8n-sandbox/bin \
		/var/lib/n8n-sandbox \
		/var/sandboxes \
		/srv/firecracker/template \
		/srv/firecracker/snapshots \
		/srv/firecracker/ci-assets \
		/srv/jailer
}

configure_persistent_ip_forward() {
	echo "==> Enabling persistent host IP forwarding..."
	cat >/etc/sysctl.d/99-n8n-sandbox-firecracker.conf <<'EOF'
net.ipv4.ip_forward = 1
EOF
	sysctl --system >/dev/null
}

configure_host_nat() {
	if [[ ! -x "$CONFIGURE_HOST_NAT_SCRIPT" ]]; then
		echo "ERROR: missing configure-host-nat script: ${CONFIGURE_HOST_NAT_SCRIPT}" >&2
		exit 1
	fi
	echo "==> Configuring host NAT and FORWARD rules..."
	"$CONFIGURE_HOST_NAT_SCRIPT"
}

prepare_runtime_dirs() {
	echo "==> Preparing Firecracker cgroups and jailer mount..."
	mkdir -p /sys/fs/cgroup/firecracker
	chown 1000:1000 /sys/fs/cgroup/firecracker
	if ! mountpoint -q /srv/jailer; then
		mount -t tmpfs -o "rw,nosuid,mode=0755,size=${JAILER_TMPFS_SIZE}" tmpfs /srv/jailer
	fi
	chown -R 1000:1000 /srv/firecracker/template /srv/firecracker/snapshots 2>/dev/null || true
}

download_ci_assets() {
	if [[ ! -x "$FIRECRACKER_CI_ASSETS_BIN" ]]; then
		echo "ERROR: missing firecracker-ci-assets script: ${FIRECRACKER_CI_ASSETS_BIN}" >&2
		exit 1
	fi
	echo "==> Downloading Firecracker CI assets for image bake..."
	FIRECRACKER_CI_VERSION="$FIRECRACKER_CI_VERSION" \
		"$FIRECRACKER_CI_ASSETS_BIN" download "$FIRECRACKER_CI_ASSETS_DIR"
	"$FIRECRACKER_CI_ASSETS_BIN" verify "$FIRECRACKER_CI_ASSETS_DIR"
}

while [[ $# -gt 0 ]]; do
	case "$1" in
	--skip-packages)
		SKIP_PACKAGES=1
		shift
		;;
	--skip-firecracker)
		SKIP_FIRECRACKER=1
		shift
		;;
	--download-ci-assets)
		DOWNLOAD_FIRECRACKER_CI_ASSETS=1
		shift
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

require_root

if [[ "$(uname -m)" != "x86_64" ]]; then
	echo "ERROR: Firecracker runner host setup supports amd64/x86_64 only; got $(uname -m)" >&2
	exit 1
fi

if [[ "$FIRECRACKER_VERSION" != v* ]]; then
	FIRECRACKER_VERSION="v${FIRECRACKER_VERSION}"
fi

if [[ "$SKIP_PACKAGES" -eq 0 ]]; then
	install_host_packages
fi

if [[ "$SKIP_FIRECRACKER" -eq 0 ]]; then
	resolve_firecracker_tarball_sha256
	install_firecracker_release
fi

prepare_firecracker_dirs
configure_persistent_ip_forward
configure_host_nat
prepare_runtime_dirs

if [[ "$DOWNLOAD_FIRECRACKER_CI_ASSETS" == "1" ]]; then
	download_ci_assets
fi

echo "==> Firecracker runner host prerequisites installed"
