#!/usr/bin/env bash
# Emits a JSON host fingerprint for snapshot compatibility analysis.
set -euo pipefail

json_escape() {
	node -e 'process.stdout.write(JSON.stringify(process.argv[1] ?? ""))' "$1"
}

kvm_param() {
	local path=$1
	if [[ -r "$path" ]]; then
		cat "$path"
	fi
}

cpu_model="$(sed -n 's/^model name[[:space:]]*: //p' /proc/cpuinfo | head -n 1)"
cpu_flags="$(sed -n 's/^flags[[:space:]]*: //p' /proc/cpuinfo | head -n 1)"
azure_vm_size="$(curl -fsSL -H 'Metadata: true' 'http://169.254.169.254/metadata/instance/compute/vmSize?api-version=2021-02-01&format=text' 2>/dev/null || true)"
azure_name="$(curl -fsSL -H 'Metadata: true' 'http://169.254.169.254/metadata/instance/compute/name?api-version=2021-02-01&format=text' 2>/dev/null || hostname)"

cat <<EOF
{
  "hostname": $(json_escape "$(hostname)"),
  "azure_name": $(json_escape "$azure_name"),
  "azure_vm_size": $(json_escape "$azure_vm_size"),
  "cpu_model": $(json_escape "$cpu_model"),
  "cpu_flags": $(json_escape "$cpu_flags"),
  "kvm_intel_nested": $(json_escape "$(kvm_param /sys/module/kvm_intel/parameters/nested)"),
  "kvm_intel_ept": $(json_escape "$(kvm_param /sys/module/kvm_intel/parameters/ept)"),
  "kvm_intel_unrestricted_guest": $(json_escape "$(kvm_param /sys/module/kvm_intel/parameters/unrestricted_guest)"),
  "kvm_intel_tsc_scaling": $(json_escape "$(kvm_param /sys/module/kvm_intel/parameters/tsc_scaling)"),
  "kvm_amd_nested": $(json_escape "$(kvm_param /sys/module/kvm_amd/parameters/nested)"),
  "collected_at": $(json_escape "$(date -u '+%Y-%m-%dT%H:%M:%SZ')")
}
EOF
