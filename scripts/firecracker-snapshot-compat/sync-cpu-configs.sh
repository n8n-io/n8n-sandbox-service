#!/usr/bin/env bash
# Rebuilds study CPU configs locally and copies them to every VM in instances.json.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
OUT_DIR="${COMPAT_OUT_DIR:-$ROOT/scripts/firecracker-snapshot-compat}"
INSTANCES_JSON="${1:-$OUT_DIR/instances.json}"
REMOTE_OPTS="-o StrictHostKeyChecking=no -o ServerAliveInterval=30"

if [[ ! -f "$INSTANCES_JSON" ]]; then
	echo "ERROR: instances file not found: ${INSTANCES_JSON}" >&2
	exit 1
fi

INSTANCES_JSON_ABS="$(cd "$(dirname "$INSTANCES_JSON")" && pwd)/$(basename "$INSTANCES_JSON")"
SSH_KEY="$(node -e 'const fs=require("fs"); const j=JSON.parse(fs.readFileSync(process.argv[1],"utf8")); process.stdout.write(j.ssh_key_path)' "$INSTANCES_JSON_ABS")"
ADMIN="$(node -e 'const fs=require("fs"); const j=JSON.parse(fs.readFileSync(process.argv[1],"utf8")); process.stdout.write(j.admin_username)' "$INSTANCES_JSON_ABS")"
INSTANCE_COUNT="$(node -e 'const fs=require("fs"); const j=JSON.parse(fs.readFileSync(process.argv[1],"utf8")); process.stdout.write(String(j.instances.length))' "$INSTANCES_JSON_ABS")"

mkdir -p "${OUT_DIR}/cpu-configs"
if ssh $REMOTE_OPTS -i "$(node -e 'const fs=require("fs"); const j=JSON.parse(fs.readFileSync(process.argv[1],"utf8")); process.stdout.write(j.ssh_key_path)' "$INSTANCES_JSON_ABS")" \
	"$(node -e 'const fs=require("fs"); const j=JSON.parse(fs.readFileSync(process.argv[1],"utf8")); process.stdout.write(j.admin_username)' "$INSTANCES_JSON_ABS")@$(node -e 'const fs=require("fs"); const j=JSON.parse(fs.readFileSync(process.argv[1],"utf8")); process.stdout.write(j.instances[0].ip)' "$INSTANCES_JSON_ABS")" \
	'test -x /opt/firecracker/bin/cpu-template-helper' 2>/dev/null; then
	bash "${ROOT}/scripts/firecracker-snapshot-compat/build-helper-configs-remote.sh" "$INSTANCES_JSON_ABS"
	INTEL_ONLY=1 bash "${ROOT}/scripts/firecracker-snapshot-compat/build-helper-configs-remote.sh" "$INSTANCES_JSON_ABS"
else
	bash "${ROOT}/scripts/firecracker-snapshot-compat/build-helper-custom-config.sh" \
		--out "${OUT_DIR}/cpu-configs/helper-custom.json" \
		--compat-dir "$OUT_DIR"
	COMPAT_OUT_DIR="$OUT_DIR" COMPAT_INSTANCES_JSON="$INSTANCES_JSON" \
		bash "${ROOT}/scripts/firecracker-snapshot-compat/build-helper-intel-config.sh" \
		--out "${OUT_DIR}/cpu-configs/helper-intel-only.json"
fi

instance_ip() {
	node -e 'const fs=require("fs"); const j=JSON.parse(fs.readFileSync(process.argv[1],"utf8")); const i=Number(process.argv[2]); process.stdout.write(j.instances.find((x)=>x.index===i)?.ip||j.instances[i]?.ip||"")' "$INSTANCES_JSON_ABS" "$1"
}

for index in $(seq 0 $((INSTANCE_COUNT - 1))); do
	ip="$(instance_ip "$index")"
	ssh $REMOTE_OPTS -i "$SSH_KEY" "${ADMIN}@${ip}" "mkdir -p /tmp/fc-compat-cpu-configs"
	scp $REMOTE_OPTS -i "$SSH_KEY" \
		"${OUT_DIR}/cpu-configs/helper-custom.json" \
		"${ADMIN}@${ip}:/tmp/fc-compat-cpu-configs/helper-custom.json"
	scp $REMOTE_OPTS -i "$SSH_KEY" \
		"${OUT_DIR}/cpu-configs/helper-intel-only.json" \
		"${ADMIN}@${ip}:/tmp/fc-compat-cpu-configs/helper-intel-only.json"
	ssh $REMOTE_OPTS -i "$SSH_KEY" "${ADMIN}@${ip}" \
		"printf '%s\n' '{\"kvm_capabilities\": [\"!56\"]}' > /tmp/fc-compat-cpu-configs/no-xcrs.json"
done

echo "==> Synced CPU configs to ${INSTANCE_COUNT} instances"
