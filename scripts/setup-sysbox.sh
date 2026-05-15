#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

SYSBOX_VERSION="0.7.0"
SYSBOX_BASE_URL="https://downloads.nestybox.com/sysbox/releases/v${SYSBOX_VERSION}"
MANUAL_INSTALL_URL="https://github.com/nestybox/sysbox/blob/master/docs/user-guide/install-package.md"
SHA256_AMD64="eeff273671467b8fa351ab3d40709759462dc03d9f7b50a1b207b37982ce40a9"
SHA256_ARM64="eae9c0e91ddd39bd1826d6a7a313a73d42a8449ef5113e9d6d118b559cb809ba"

DL_DIR=""

cleanup() {
	if [[ -n "$DL_DIR" && -d "$DL_DIR" ]]; then
		rm -rf "$DL_DIR"
	fi
}
trap cleanup EXIT

err() {
	echo "ERROR: $1" >&2
	echo "For manual installation, see: ${MANUAL_INSTALL_URL}" >&2
	exit 1
}

detect_arch() {
	local arch
	arch=$(uname -m | sed 's/aarch64/arm64/' | sed 's/x86_64/amd64/')
	case "$arch" in
		amd64|arm64) echo "$arch" ;;
		*) err "Unsupported architecture: $(uname -m). Only amd64 and arm64 are supported." ;;
	esac
}

check_distro() {
	if [[ ! -f /etc/os-release ]]; then
		err "Cannot detect distribution: /etc/os-release not found."
	fi

	# shellcheck source=/dev/null
	. /etc/os-release

	local major_version="${VERSION_ID%%.*}"

	case "$ID" in
		ubuntu)
			case "$major_version" in
				18|20|22|24) ;;
				*) err "Unsupported Ubuntu version: ${VERSION_ID}. Supported: 18, 20, 22, 24." ;;
			esac
			;;
		debian)
			case "$major_version" in
				10|11) ;;
				*) err "Unsupported Debian version: ${VERSION_ID}. Supported: 10, 11." ;;
			esac
			;;
		*) err "Unsupported distribution: ${ID}. Only Ubuntu and Debian are supported." ;;
	esac

	echo "==> Distribution: ${ID} ${VERSION_ID}"
}

check_kernel() {
	local kernel major minor
	kernel=$(uname -r)
	IFS='.-' read -r major minor _ <<< "$kernel"

	if [[ "$major" -lt 5 ]] || { [[ "$major" -eq 5 ]] && [[ "$minor" -le 19 ]]; }; then
		err "Kernel version ${kernel} is too old. Sysbox requires kernel > 5.19."
	fi

	echo "==> Kernel: ${kernel}"
}

check_docker() {
	if ! command -v docker >/dev/null 2>&1; then
		err "Docker is not installed. Please install Docker first."
	fi
	echo "==> Docker: installed"
}

check_sudo() {
	if ! sudo -v 2>/dev/null; then
		echo "ERROR: sudo access is required to install sysbox." >&2
		exit 1
	fi
}

check_sysbox_installed() {
	if command -v sysbox-runc >/dev/null 2>&1 && systemctl is-active --quiet sysbox 2>/dev/null; then
		echo "==> Sysbox is already installed and running. Nothing to do."
		exit 0
	fi
}

install_jq() {
	if command -v jq >/dev/null 2>&1; then
		echo "==> jq already installed, skipping"
		return
	fi
	echo "==> Installing jq..."
	sudo apt-get update -qq
	sudo apt-get install -y -qq jq
}

ensure_br_netfilter() {
	if lsmod | grep -q br_netfilter; then
		echo "==> br_netfilter module already loaded, skipping"
	else
		echo "==> Loading br_netfilter kernel module..."
		sudo modprobe br_netfilter
	fi

	if [[ ! -f /etc/modules-load.d/br_netfilter.conf ]]; then
		echo "==> Persisting br_netfilter module across reboots..."
		echo "br_netfilter" | sudo tee /etc/modules-load.d/br_netfilter.conf >/dev/null
	fi
}

download_sysbox() {
	local arch="$1"
	local filename="sysbox-ce_${SYSBOX_VERSION}-0.linux_${arch}.deb"
	local url="${SYSBOX_BASE_URL}/${filename}"
	local expected_sha

	case "$arch" in
		amd64) expected_sha="$SHA256_AMD64" ;;
		arm64) expected_sha="$SHA256_ARM64" ;;
	esac

	DL_DIR=$(mktemp -d)
	DL_FILE="${DL_DIR}/${filename}"

	echo "==> Downloading ${filename}..."
	curl -fSL -o "$DL_FILE" "$url"

	echo "==> Verifying SHA256 checksum..."
	echo "${expected_sha}  ${DL_FILE}" | sha256sum -c - >/dev/null
	echo "==> Checksum verified"
}

stop_docker_containers() {
	local running
	running=$(docker ps -q) || true

	if [[ -z "$running" ]]; then
		return
	fi

	echo "==> Running Docker containers detected:"
	docker ps --format 'table {{.ID}}\t{{.Image}}\t{{.Names}}'
	echo ""
	read -rp "All containers must be stopped and removed to install sysbox. Proceed? [y/N] " answer

	if [[ "$answer" != "y" && "$answer" != "Y" ]]; then
		echo "Aborting installation." >&2
		exit 1
	fi

	echo "==> Stopping all containers..."
	docker ps -q | xargs --no-run-if-empty docker stop
	echo "==> Removing all containers..."
	docker ps -aq | xargs --no-run-if-empty docker rm 2>/dev/null || true
}

install_sysbox() {
	echo "==> Installing sysbox..."
	sudo apt-get install -y "$DL_FILE"
}

verify_sysbox() {
	if ! command -v sysbox-runc >/dev/null 2>&1; then
		echo "ERROR: sysbox-runc binary not found after installation." >&2
		exit 1
	fi

	if ! systemctl is-active --quiet sysbox; then
		echo "ERROR: sysbox service is not running." >&2
		exit 1
	fi

	if docker info --format '{{json .Runtimes}}' | jq -e '.["sysbox-runc"]' >/dev/null 2>&1; then
		echo "==> Docker runtime sysbox-runc is registered"
	else
		echo "ERROR: sysbox-runc runtime not registered with Docker." >&2
		exit 1
	fi
}

main() {
	echo "==> Setting up sysbox v${SYSBOX_VERSION}"

	check_sysbox_installed

	echo "==> Checking prerequisites..."
	local arch
	arch=$(detect_arch)
	check_distro
	check_kernel
	check_docker
	check_sudo

	echo "==> Installing dependencies..."
	install_jq
	ensure_br_netfilter

	echo "==> Downloading sysbox..."
	download_sysbox "$arch"

	stop_docker_containers
	install_sysbox

	echo "==> Verifying sysbox installation..."
	verify_sysbox

	echo "==> Sysbox v${SYSBOX_VERSION} installed successfully!"
}

main "$@"
