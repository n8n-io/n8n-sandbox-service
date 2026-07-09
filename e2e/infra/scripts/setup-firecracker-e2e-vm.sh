#!/usr/bin/env bash
# Installs the host dependencies required by the Firecracker runner and
# builds the kernel/rootfs template and snapshot locally on the VM.
# Intended to run ON the VM after the project source has been copied to ~/project.
set -euxo pipefail

GO_VERSION="${GO_VERSION:-1.25.0}"
NODE_MAJOR="${NODE_MAJOR:-24}"
PNPM_VERSION="${PNPM_VERSION:-10}"
FIRECRACKER_VERSION="${FIRECRACKER_VERSION:-v1.14.1}"
FIRECRACKER_TARBALL_SHA256="${FIRECRACKER_TARBALL_SHA256:-}"
JAILER_TMPFS_SIZE="${JAILER_TMPFS_SIZE:-8G}"
FIRECRACKER_CI_VERSION="${FIRECRACKER_CI_VERSION:-${FIRECRACKER_VERSION%.*}}"
FIRECRACKER_E2E_ROOTFS_SIZE_MB="${FIRECRACKER_E2E_ROOTFS_SIZE_MB:-1024}"
FIRECRACKER_E2E_SNAPSHOT_MEM_MIB="${FIRECRACKER_E2E_SNAPSHOT_MEM_MIB:-512}"
FIRECRACKER_E2E_SNAPSHOT_VCPUS="${FIRECRACKER_E2E_SNAPSHOT_VCPUS:-1}"

if [[ "$(uname -m)" != "x86_64" ]]; then
	echo "Firecracker e2e assets are currently amd64/x86_64 only; got $(uname -m)" >&2
	exit 1
fi

if [[ "$FIRECRACKER_VERSION" != v* ]]; then
	FIRECRACKER_VERSION="v${FIRECRACKER_VERSION}"
fi

# Reads the Azure VM size from the instance metadata service for diagnostics.
current_azure_vm_size() {
	curl -fsSL \
		-H "Metadata: true" \
		"http://169.254.169.254/metadata/instance/compute/vmSize?api-version=2021-02-01&format=text" \
		2>/dev/null || true
}

# Returns the host CPU model reported by Linux. This is written to the manifest
# because Firecracker snapshots are sensitive to the guest-visible CPU feature set.
current_cpu_model() {
	sed -n 's/^model name[[:space:]]*: //p' /proc/cpuinfo | head -n 1
}

# Returns the host CPU flags reported by Linux for preflight and diagnostics.
current_cpu_flags() {
	sed -n 's/^flags[[:space:]]*: //p' /proc/cpuinfo | head -n 1
}

# Checks one KVM module parameter and records its value. The Firecracker e2e VM
# requires nested virtualization support from the Azure host.
require_kvm_param() {
	local path=$1 label=$2 expected_regex=$3 value
	if [[ ! -r "$path" ]]; then
		echo "ERROR: missing required KVM parameter for Firecracker: $label ($path)" >&2
		return 1
	fi

	value="$(cat "$path")"
	echo "$path=$value"
	if [[ ! "$value" =~ $expected_regex ]]; then
		echo "ERROR: $label must match ${expected_regex}; got ${value}" >&2
		return 1
	fi
}

# Fails fast when the Azure VM cannot run nested Firecracker microVMs. Without
# these checks, setup can fail much later while booting the snapshot builder VM.
firecracker_host_preflight() {
	local failed=0 flags cpu_model

	echo "==> Checking Firecracker host KVM capabilities..."
	cpu_model="$(current_cpu_model || true)"
	flags="$(current_cpu_flags || true)"
	echo "cpu: ${cpu_model:-unknown}"

	if [[ ! -e /dev/kvm ]]; then
		echo "ERROR: /dev/kvm is not available. Choose an Azure VM size with nested virtualization support." >&2
		failed=1
	elif [[ ! -r /dev/kvm || ! -w /dev/kvm ]]; then
		ls -l /dev/kvm >&2 || true
		echo "WARN: /dev/kvm is not readable and writable by $(id -un); Firecracker e2e setup runs snapshot creation and the runner via sudo." >&2
	else
		ls -l /dev/kvm
	fi

	if [[ " ${flags} " != *" vmx "* && " ${flags} " != *" svm "* ]]; then
		echo "ERROR: CPU flags do not expose vmx or svm nested virtualization support." >&2
		failed=1
	fi

	if [[ -d /sys/module/kvm_intel || " ${flags} " == *" vmx "* ]]; then
		require_kvm_param /sys/module/kvm_intel/parameters/nested "Intel nested virtualization" '^(Y|1)$' || failed=1
		require_kvm_param /sys/module/kvm_intel/parameters/ept "Intel EPT" '^(Y|1)$' || failed=1
	elif [[ -d /sys/module/kvm_amd || " ${flags} " == *" svm "* ]]; then
		require_kvm_param /sys/module/kvm_amd/parameters/nested "AMD nested virtualization" '^(Y|1)$' || failed=1
	else
		echo "ERROR: neither kvm_intel nor kvm_amd appears to be loaded." >&2
		failed=1
	fi

	if [[ "$failed" -ne 0 ]]; then
		echo "ERROR: Firecracker host preflight failed; discard this VM and provision another host." >&2
		exit 1
	fi
}

# Reads whether KVM TSC scaling is enabled when the host exposes that parameter.
# The value is diagnostic context for snapshot compatibility problems.
current_kvm_tsc_scaling() {
	if [[ -r /sys/module/kvm_intel/parameters/tsc_scaling ]]; then
		cat /sys/module/kvm_intel/parameters/tsc_scaling
	elif [[ -r /sys/module/kvm_amd/parameters/tsc_scaling ]]; then
		cat /sys/module/kvm_amd/parameters/tsc_scaling
	fi
}

# Returns a compact file(1) description when available for manifest diagnostics.
file_type() {
	if command -v file >/dev/null 2>&1; then
		file -b "$1"
	fi
}

# JSON-encodes a shell value before embedding it in the generated manifest.
json_escape() {
	node -e 'process.stdout.write(JSON.stringify(process.argv[1] ?? ""))' "$1"
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Builds the local kernel/rootfs template from upstream Firecracker CI assets.
build_template_assets() {
	sudo rm -rf /srv/firecracker/snapshots
	sudo mkdir -p /srv/firecracker/snapshots
	FIRECRACKER_CI_VERSION="$FIRECRACKER_CI_VERSION" \
		FIRECRACKER_ROOTFS_SIZE_MB="$FIRECRACKER_E2E_ROOTFS_SIZE_MB" \
		TEMPLATE_DIR="/srv/firecracker/template" \
		bash "${SCRIPT_DIR}/build-rootfs-template.sh"
}

# Writes a manifest describing exactly which host and asset inputs produced the
# local template/snapshot. This is collected with e2e failure artifacts.
write_manifest() {
	local manifest=$1
	sudo tee "$manifest" >/dev/null <<EOF
{
  "firecracker_version": $(json_escape "${FIRECRACKER_VERSION#v}"),
  "firecracker_ci_version": $(json_escape "${FIRECRACKER_CI_VERSION}"),
  "azure_vm_size": $(json_escape "$(current_azure_vm_size)"),
  "cpu_model": $(json_escape "$(current_cpu_model)"),
  "cpu_flags": $(json_escape "$(current_cpu_flags)"),
  "kvm_tsc_scaling": $(json_escape "$(current_kvm_tsc_scaling)"),
  "kernel_file_type": $(json_escape "$(file_type /srv/firecracker/template/vmlinux)"),
  "rootfs_file_type": $(json_escape "$(file_type /srv/firecracker/template/rootfs.ext4)"),
  "created_at": $(json_escape "$(date -u '+%Y-%m-%dT%H:%M:%SZ')")
}
EOF
}

firecracker_host_preflight

sudo env \
	FIRECRACKER_VERSION="$FIRECRACKER_VERSION" \
	FIRECRACKER_TARBALL_SHA256="${FIRECRACKER_TARBALL_SHA256:-}" \
	JAILER_TMPFS_SIZE="$JAILER_TMPFS_SIZE" \
	FIRECRACKER_CI_VERSION="$FIRECRACKER_CI_VERSION" \
	CONFIGURE_HOST_NAT_SCRIPT="${SCRIPT_DIR}/configure-host-nat.sh" \
	FIRECRACKER_CI_ASSETS_BIN="${SCRIPT_DIR}/firecracker-ci-assets.sh" \
	bash "${SCRIPT_DIR}/install-runner-host.sh"

echo "==> Installing Go ${GO_VERSION}..."
curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -o /tmp/go.tar.gz
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf /tmp/go.tar.gz
rm /tmp/go.tar.gz
echo 'export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH' >> ~/.bashrc
export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH
sudo ln -sf /usr/local/go/bin/go /usr/local/bin/go
sudo ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
go version

echo "==> Installing Node.js ${NODE_MAJOR} and pnpm ${PNPM_VERSION}..."
curl -fsSL "https://deb.nodesource.com/setup_${NODE_MAJOR}.x" | sudo -E bash -
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq nodejs
sudo npm install -g "pnpm@${PNPM_VERSION}"
node --version
pnpm --version

build_template_assets
write_manifest /srv/firecracker/manifest.json
sudo chown -R 1000:1000 /srv/firecracker/template /srv/firecracker/snapshots
sudo chmod 0755 /srv/firecracker /srv/firecracker/template /srv/firecracker/snapshots
sudo chmod 0664 /srv/firecracker/template/rootfs.ext4
sudo chmod 0644 /srv/firecracker/template/vmlinux

echo "==> Building sandbox daemon for local Firecracker snapshot..."
cd ~/project
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/sandbox-daemon ./cmd/daemon

echo "==> Creating Firecracker e2e snapshot on this VM..."
sudo env \
	MEM_MIB="$FIRECRACKER_E2E_SNAPSHOT_MEM_MIB" \
	VCPUS="$FIRECRACKER_E2E_SNAPSHOT_VCPUS" \
	bash e2e/infra/scripts/create-golden-snapshot.sh \
	--kernel /srv/firecracker/template/vmlinux \
	--ext4 /srv/firecracker/template/rootfs.ext4 \
	--daemon-bin ./bin/sandbox-daemon \
	--out /srv/firecracker/snapshots

sudo ln -sf snapshot_mem /srv/firecracker/snapshots/mem
sudo ln -sf snapshot_state /srv/firecracker/snapshots/state
sudo chown -R 1000:1000 /srv/firecracker/template /srv/firecracker/snapshots
sudo chmod 0644 /srv/firecracker/snapshots/snapshot_mem /srv/firecracker/snapshots/snapshot_state

echo "==> Firecracker VM setup complete"
