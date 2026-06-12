#!/usr/bin/env bash
# Installs cpu-template-helper from the pinned Firecracker release on study VMs.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
OUT_DIR="${COMPAT_OUT_DIR:-$ROOT/scripts/firecracker-snapshot-compat}"
INSTANCES_JSON="${1:-$OUT_DIR/instances.json}"
FIRECRACKER_VERSION="${FIRECRACKER_VERSION:-v1.14.1}"
REMOTE_OPTS="-o StrictHostKeyChecking=no -o ConnectTimeout=10 -o ServerAliveInterval=30"

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

install_local() {
	local dest="${1:-/opt/firecracker/bin/cpu-template-helper}"
	if [[ -x "$dest" ]]; then
		echo "==> cpu-template-helper already installed at ${dest}"
		return 0
	fi
	local tmp
	tmp="$(mktemp -d)"
	trap 'rm -rf "$tmp"' RETURN
	curl -fsSL \
		"https://github.com/firecracker-microvm/firecracker/releases/download/${FIRECRACKER_VERSION}/firecracker-${FIRECRACKER_VERSION}-x86_64.tgz" \
		-o "$tmp/firecracker.tgz"
	tar xzf "$tmp/firecracker.tgz" -C "$tmp"
	sudo install -m 0755 -d "$(dirname "$dest")"
	sudo install -m 0755 \
		"$tmp/release-${FIRECRACKER_VERSION}-x86_64/cpu-template-helper-${FIRECRACKER_VERSION}-x86_64" \
		"$dest"
	echo "==> Installed cpu-template-helper to ${dest}"
}

if [[ "${INSTALL_LOCAL:-0}" == "1" ]]; then
	install_local "${CPU_TEMPLATE_HELPER:-$HOME/.local/bin/cpu-template-helper}"
	exit 0
fi

HELPER_TAR="$(mktemp -t fc-helper.XXXXXX.tgz)"
tmp="$(mktemp -d)"
curl -fsSL \
	"https://github.com/firecracker-microvm/firecracker/releases/download/${FIRECRACKER_VERSION}/firecracker-${FIRECRACKER_VERSION}-x86_64.tgz" \
	-o "$tmp/firecracker.tgz"
tar xzf "$tmp/firecracker.tgz" -C "$tmp"
HELPER_BIN="$tmp/release-${FIRECRACKER_VERSION}-x86_64/cpu-template-helper-${FIRECRACKER_VERSION}-x86_64"
tar czf "$HELPER_TAR" -C "$(dirname "$HELPER_BIN")" "$(basename "$HELPER_BIN")"
rm -rf "$tmp"

for index in $(seq 0 $((INSTANCE_COUNT - 1))); do
	ip="$(instance_ip "$index")"
	echo "==> Installing on instance ${index} (${ip})..." >&2
	scp $REMOTE_OPTS -i "$SSH_KEY" "$HELPER_TAR" "${ADMIN}@${ip}:/tmp/cpu-template-helper.tgz"
	ssh $REMOTE_OPTS -i "$SSH_KEY" "${ADMIN}@${ip}" bash -s <<'REMOTE'
set -euo pipefail
sudo install -m 0755 -d /opt/firecracker/bin
tar xzf /tmp/cpu-template-helper.tgz -C /tmp
sudo install -m 0755 /tmp/cpu-template-helper-* /opt/firecracker/bin/cpu-template-helper
rm -f /tmp/cpu-template-helper.tgz /tmp/cpu-template-helper-*
/opt/firecracker/bin/cpu-template-helper --help >/dev/null 2>&1 || /opt/firecracker/bin/cpu-template-helper 2>&1 | head -1
REMOTE
done
rm -f "$HELPER_TAR"

echo "==> cpu-template-helper installed on ${INSTANCE_COUNT} instances"
