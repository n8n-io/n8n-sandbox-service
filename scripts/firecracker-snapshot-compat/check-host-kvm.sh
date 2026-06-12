#!/usr/bin/env bash
# Collects KVM / TSC diagnostics from every VM in instances.json.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
OUT_DIR="${COMPAT_OUT_DIR:-$ROOT/scripts/firecracker-snapshot-compat}"
INSTANCES_JSON="${1:-$OUT_DIR/instances.json}"
REMOTE_OPTS="-o StrictHostKeyChecking=no -o ConnectTimeout=10 -o ServerAliveInterval=30"
OUT_FILE="${2:-$OUT_DIR/host-kvm-diagnostics.json}"

if [[ ! -f "$INSTANCES_JSON" ]]; then
	echo "ERROR: instances file not found: ${INSTANCES_JSON}" >&2
	exit 1
fi

SSH_KEY="$(node -e 'const j=require(process.argv[1]); process.stdout.write(j.ssh_key_path)' "$INSTANCES_JSON")"
ADMIN="$(node -e 'const j=require(process.argv[1]); process.stdout.write(j.admin_username)' "$INSTANCES_JSON")"
INSTANCE_COUNT="$(node -e 'const j=require(process.argv[1]); process.stdout.write(String(j.instances.length))' "$INSTANCES_JSON")"

instance_ip() {
	node -e 'const j=require(process.argv[1]); const i=Number(process.argv[2]); process.stdout.write(j.instances.find((x)=>x.index===i)?.ip||j.instances[i]?.ip||"")' "$INSTANCES_JSON" "$1"
}

instance_meta() {
	node -e 'const j=require(process.argv[1]); const i=Number(process.argv[2]); const inst=j.instances.find((x)=>x.index===i)||j.instances[i]; process.stdout.write(JSON.stringify(inst||{}))' "$INSTANCES_JSON" "$1"
}

REMOTE_SCRIPT='read_param() { [[ -r "$1" ]] && cat "$1" || echo ""; }
echo "{"
echo "  \"hostname\": \"$(hostname)\","
echo "  \"cpu_model\": \"$(sed -n "s/^model name[[:space:]]*: //p" /proc/cpuinfo | head -n1)\","
echo "  \"kvm_intel_tsc_scaling\": \"$(read_param /sys/module/kvm_intel/parameters/tsc_scaling)\","
echo "  \"kvm_intel_nested\": \"$(read_param /sys/module/kvm_intel/parameters/nested)\","
echo "  \"kvm_intel_ept\": \"$(read_param /sys/module/kvm_intel/parameters/ept)\","
echo "  \"has_kvm\": $([[ -e /dev/kvm ]] && echo true || echo false),"
echo "  \"cpu_template_helper\": $([[ -x /opt/firecracker/bin/cpu-template-helper ]] && echo true || echo false),"
echo "  \"firecracker_version\": \"$(/opt/firecracker/bin/firecracker --version 2>/dev/null | head -n1 || true)\","
echo "  \"kvm_intel_param_count\": $(ls /sys/module/kvm_intel/parameters 2>/dev/null | wc -l | tr -d "[:space:]")"
echo "}"'

results=()
for index in $(seq 0 $((INSTANCE_COUNT - 1))); do
	ip="$(instance_ip "$index")"
	meta="$(instance_meta "$index")"
	echo "==> Checking instance ${index} (${ip})..." >&2
	remote_json="$(ssh $REMOTE_OPTS -i "$SSH_KEY" "${ADMIN}@${ip}" "bash -lc $(printf '%q' "$REMOTE_SCRIPT")")"
	results+=("$(node -e 'const meta=JSON.parse(process.argv[1]); const remote=JSON.parse(process.argv[2]); console.log(JSON.stringify({...meta, diagnostics:remote}));' "$meta" "$remote_json")")
done

node -e '
const rows = process.argv.slice(1).map(JSON.parse);
const out = {
  collected_at: new Date().toISOString(),
  instance_count: rows.length,
  instances: rows,
};
console.log(JSON.stringify(out, null, 2));
' "${results[@]}" >"$OUT_FILE"

echo "==> Wrote ${OUT_FILE}"
