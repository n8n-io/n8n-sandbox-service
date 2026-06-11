#!/usr/bin/env bash
# Collects Firecracker e2e diagnostics from the Azure VM after a failure.
# Requires: VM_IP and SSH_KEY_PATH env vars pointing to the running VM.
# Downloads artifacts to /tmp/e2e-firecracker-artifacts/ on the local machine.
set -euo pipefail

: "${VM_IP:?VM_IP is required}"
: "${SSH_KEY_PATH:?SSH_KEY_PATH is required}"

VM_ADMIN="azureuser"
SSH_OPTS="-o StrictHostKeyChecking=no -o ConnectTimeout=10"
ARTIFACT_DIR="/tmp/e2e-firecracker-artifacts"

echo "==> Collecting Firecracker logs from ${VM_IP}..."

ssh $SSH_OPTS -i "$SSH_KEY_PATH" "${VM_ADMIN}@${VM_IP}" bash -s << 'EOF'
set -x
rm -rf /tmp/e2e-firecracker-artifacts
mkdir -p /tmp/e2e-firecracker-artifacts
cd ~/project
tar czf /tmp/e2e-firecracker-artifacts/e2e-results.tar.gz e2e/test-results/ 2>/dev/null || true
cp /tmp/sandbox-api-firecracker-e2e.log /tmp/e2e-firecracker-artifacts/api.log 2>/dev/null || true
sudo cp /tmp/sandbox-runner-firecracker-e2e.log /tmp/e2e-firecracker-artifacts/runner.log 2>/dev/null || true
sudo chown -R "$(id -u):$(id -g)" /tmp/e2e-firecracker-artifacts

{
	echo "== uname =="
	uname -a
	echo
	echo "== id =="
	id
	echo
	echo "== /dev/kvm =="
	ls -l /dev/kvm || true
	echo
	echo "== firecracker binaries =="
	ls -l /opt/firecracker/bin || true
	/opt/firecracker/bin/firecracker --version || true
	echo
	echo "== firecracker assets =="
	sudo find /srv/firecracker -maxdepth 3 -ls || true
	echo
	echo "== jailer dir =="
	mount | grep /srv/jailer || true
	sudo find /srv/jailer -maxdepth 4 -ls || true
	echo
	echo "== cgroup =="
	sudo find /sys/fs/cgroup/firecracker -maxdepth 2 -ls || true
	echo
	echo "== netns =="
	sudo ip netns list || true
	echo
	echo "== links =="
	ip link || true
	echo
	echo "== routes =="
	ip route || true
	echo
	echo "== nat rules =="
	sudo iptables -t nat -S || true
	echo
	echo "== kernel log =="
	sudo journalctl -k --no-pager -n 200 || true
} > /tmp/e2e-firecracker-artifacts/firecracker-host-diagnostics.txt 2>&1
EOF

mkdir -p "$ARTIFACT_DIR"
scp $SSH_OPTS -i "$SSH_KEY_PATH" \
	"${VM_ADMIN}@${VM_IP}:/tmp/e2e-firecracker-artifacts/*" \
	"${ARTIFACT_DIR}/" 2>/dev/null || true

echo "==> Firecracker logs collected to ${ARTIFACT_DIR}"
