#!/usr/bin/env bash
# Builds helper CPU configs via cpu-template-helper v1.14+ on study VMs (template dump + strip).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
OUT_DIR="${COMPAT_OUT_DIR:-$ROOT/scripts/firecracker-snapshot-compat}"
INSTANCES_JSON="${1:-$OUT_DIR/instances.json}"
INTEL_ONLY="${INTEL_ONLY:-0}"
REMOTE_OPTS="-o StrictHostKeyChecking=no -o ConnectTimeout=15 -o ServerAliveInterval=30"
HELPER="/opt/firecracker/bin/cpu-template-helper"
DUMP_DIR="${OUT_DIR}/cpu-configs/dumps"
CUSTOM_OUT="${OUT_DIR}/cpu-configs/helper-custom.json"
INTEL_OUT="${OUT_DIR}/cpu-configs/helper-intel-only.json"

if [[ ! -f "$INSTANCES_JSON" ]]; then
	echo "ERROR: instances file not found: ${INSTANCES_JSON}" >&2
	exit 1
fi

SSH_KEY="$(node -e 'const fs=require("fs"); const j=JSON.parse(fs.readFileSync(process.argv[1],"utf8")); process.stdout.write(j.ssh_key_path)' "$INSTANCES_JSON")"
ADMIN="$(node -e 'const fs=require("fs"); const j=JSON.parse(fs.readFileSync(process.argv[1],"utf8")); process.stdout.write(j.admin_username)' "$INSTANCES_JSON")"

instance_ip() {
	node -e 'const fs=require("fs"); const j=JSON.parse(fs.readFileSync(process.argv[1],"utf8")); const i=Number(process.argv[2]); process.stdout.write(j.instances.find((x)=>x.index===i)?.ip||j.instances[i]?.ip||"")' "$INSTANCES_JSON" "$1"
}

REPRESENTATIVES=()
while IFS= read -r idx; do
	[[ -n "$idx" ]] && REPRESENTATIVES+=("$idx")
done < <(node -e '
const fs = require("fs");
const j = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
const intelOnly = process.argv[2] === "1";
const seen = new Map();
for (const inst of j.instances) {
  if (intelOnly && inst.cpu_model && !/Intel/i.test(inst.cpu_model)) continue;
  if (!seen.has(inst.cpu_signature)) seen.set(inst.cpu_signature, inst.index);
}
for (const idx of seen.values()) console.log(idx);
' "$INSTANCES_JSON" "$INTEL_ONLY")

mkdir -p "$DUMP_DIR"
dump_paths=()

for index in "${REPRESENTATIVES[@]}"; do
	sig="$(node -e 'const fs=require("fs"); const j=JSON.parse(fs.readFileSync(process.argv[1],"utf8")); const i=Number(process.argv[2]); const inst=j.instances.find((x)=>x.index===i); process.stdout.write(inst?.cpu_signature||"")' "$INSTANCES_JSON" "$index")"
	ip="$(instance_ip "$index")"
	remote_dump="/tmp/fc-cpu-dump-${sig}.json"
	local_dump="${DUMP_DIR}/${sig}.json"
	echo "==> template dump on instance ${index} (${sig})..." >&2
	ssh $REMOTE_OPTS -i "$SSH_KEY" "${ADMIN}@${ip}" \
		"sudo ${HELPER} template dump --output '${remote_dump}'"
	scp $REMOTE_OPTS -i "$SSH_KEY" "${ADMIN}@${ip}:${remote_dump}" "$local_dump"
	dump_paths+=("$local_dump")
done

if [[ ${#dump_paths[@]} -lt 1 ]]; then
	echo "ERROR: no template dumps collected" >&2
	exit 1
fi

intersect_out="${OUT_DIR}/cpu-configs/intersected.json"
echo "==> intersecting ${#dump_paths[@]} template dumps..." >&2
node "${ROOT}/scripts/firecracker-snapshot-compat/lib/intersect-cpu-configs.js" \
	"$intersect_out" \
	"${dump_paths[@]}"

if [[ "$INTEL_ONLY" == "1" ]]; then
	cp "$intersect_out" "$INTEL_OUT"
	echo "==> Wrote ${INTEL_OUT}"
else
	cp "$intersect_out" "$CUSTOM_OUT"
	echo "==> Wrote ${CUSTOM_OUT}"
fi
