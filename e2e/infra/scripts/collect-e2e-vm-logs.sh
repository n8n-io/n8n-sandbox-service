#!/usr/bin/env bash
# Collects e2e test logs and diagnostics from the Azure VM after a failure.
# Requires: VM_IP and SSH_KEY_PATH env vars pointing to the running VM.
# Downloads artifacts to /tmp/e2e-artifacts/ on the local machine.
set -euo pipefail

: "${VM_IP:?VM_IP is required}"
: "${SSH_KEY_PATH:?SSH_KEY_PATH is required}"

VM_ADMIN="azureuser"
SSH_OPTS="-o StrictHostKeyChecking=no -o ConnectTimeout=10"
ARTIFACT_DIR="/tmp/e2e-artifacts"

echo "==> Collecting logs from ${VM_IP}..."

ssh $SSH_OPTS -i "$SSH_KEY_PATH" "${VM_ADMIN}@${VM_IP}" bash -s << 'EOF'
set -x
cd ~/project
tar czf /tmp/e2e-results.tar.gz e2e/test-results/ 2>/dev/null || true
docker info > /tmp/docker-info.txt 2>&1 || true
sudo systemctl status sysbox > /tmp/sysbox-status.txt 2>&1 || true
sudo journalctl -u sysbox --no-pager -n 100 > /tmp/sysbox-journal.txt 2>&1 || true
EOF

mkdir -p "$ARTIFACT_DIR"
for f in e2e-results.tar.gz docker-info.txt sysbox-status.txt sysbox-journal.txt; do
	scp $SSH_OPTS -i "$SSH_KEY_PATH" "${VM_ADMIN}@${VM_IP}:/tmp/${f}" "${ARTIFACT_DIR}/" 2>/dev/null || true
done

echo "==> Logs collected to ${ARTIFACT_DIR}"
