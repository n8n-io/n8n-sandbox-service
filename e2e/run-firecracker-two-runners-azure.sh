#!/usr/bin/env bash
# Orchestrates Firecracker two-runner e2e across a control VM and a peer runner VM.
# Requires a prior provision with E2E_PEER_VM_ENABLED=true.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
# shellcheck source=e2e/lib/common.sh
source "$SCRIPT_DIR/lib/common.sh"

: "${VM_IP:?VM_IP is required}"
: "${VM_PRIVATE_IP:?VM_PRIVATE_IP is required}"
: "${PEER_VM_PRIVATE_IP:?PEER_VM_PRIVATE_IP is required}"
: "${SSH_KEY_PATH:?SSH_KEY_PATH is required}"

VM_ADMIN="${VM_ADMIN:-azureuser}"
SSH_OPTS="-o StrictHostKeyChecking=no -o ServerAliveInterval=30 -o ServerAliveCountMax=6"
TLS_DIR="$(mktemp -d)"
REMOTE_TLS_DIR="/tmp/n8n-sandbox-e2e-tls-$$"
RUNNER2_PID=""

cleanup() {
	rm -rf "$TLS_DIR"
}
trap cleanup EXIT

echo "Bootstrapping shared mTLS material..."
bash "$PROJECT_DIR/scripts/bootstrap-mtls.sh" \
	--out-dir "$TLS_DIR" \
	--api-san sandbox-api-e2e-mtls \
	--control-sans "runner-control-a,runner-control-b,localhost"

echo "Copying TLS material to VMs..."
ssh $SSH_OPTS -i "$SSH_KEY_PATH" "${VM_ADMIN}@${VM_IP}" "rm -rf '$REMOTE_TLS_DIR' && mkdir -p '$REMOTE_TLS_DIR'"
e2e_ssh_peer "$VM_IP" "$PEER_VM_PRIVATE_IP" "$SSH_KEY_PATH" "$VM_ADMIN" \
	"rm -rf '$REMOTE_TLS_DIR' && mkdir -p '$REMOTE_TLS_DIR'"
scp $SSH_OPTS -i "$SSH_KEY_PATH" -r "$TLS_DIR/." "${VM_ADMIN}@${VM_IP}:${REMOTE_TLS_DIR}/"
e2e_scp_dir_to_peer "$VM_IP" "$PEER_VM_PRIVATE_IP" "$SSH_KEY_PATH" "$VM_ADMIN" \
	"$TLS_DIR/." "${REMOTE_TLS_DIR}/"

echo "Installing SSH key on control VM for peer runner management..."
scp $SSH_OPTS -i "$SSH_KEY_PATH" "$SSH_KEY_PATH" "${VM_ADMIN}@${VM_IP}:~/.ssh/e2e-peer-key"
ssh $SSH_OPTS -i "$SSH_KEY_PATH" "${VM_ADMIN}@${VM_IP}" "chmod 600 ~/.ssh/e2e-peer-key"

echo "Starting peer Firecracker runner on ${PEER_VM_PRIVATE_IP}..."
e2e_ssh_peer "$VM_IP" "$PEER_VM_PRIVATE_IP" "$SSH_KEY_PATH" "$VM_ADMIN" bash -s <<EOF
set -euo pipefail
export PATH=/usr/local/go/bin:\$HOME/go/bin:\$PATH
cd ~/project
E2E_TLS_DIR='$REMOTE_TLS_DIR' \
E2E_CONTROL_PRIVATE_IP='$VM_PRIVATE_IP' \
E2E_PEER_PRIVATE_IP='$PEER_VM_PRIVATE_IP' \
bash e2e/run-firecracker-runner-peer.sh
EOF

RUNNER2_PID=$(e2e_ssh_peer "$VM_IP" "$PEER_VM_PRIVATE_IP" "$SSH_KEY_PATH" "$VM_ADMIN" \
	"cat ~/project/e2e/.fc-runner-b.pid")

echo "Running two-runner tests on control VM ${VM_IP}..."
ssh $SSH_OPTS -i "$SSH_KEY_PATH" "${VM_ADMIN}@${VM_IP}" bash -s -- "$@" <<EOF
set -euo pipefail
export PATH=/usr/local/go/bin:\$HOME/go/bin:\$PATH
cd ~/project
E2E_TLS_DIR='$REMOTE_TLS_DIR' \
E2E_CONTROL_PRIVATE_IP='$VM_PRIVATE_IP' \
E2E_PEER_PRIVATE_IP='$PEER_VM_PRIVATE_IP' \
E2E_RUNNER2_HTTP_ADDR='${PEER_VM_PRIVATE_IP}:18085' \
E2E_RUNNER2_PID='${RUNNER2_PID}' \
E2E_RUNNER2_ENV_FILE='/home/${VM_ADMIN}/project/e2e/.fc-runner-b.env' \
E2E_RUNNER2_REMOTE_SSH='${VM_ADMIN}@${PEER_VM_PRIVATE_IP}' \
E2E_RUNNER2_REMOTE_SSH_OPTS='-i /home/${VM_ADMIN}/.ssh/e2e-peer-key -o StrictHostKeyChecking=no' \
bash e2e/run-firecracker-two-runners.sh "\$@"
EOF

echo "Stopping peer Firecracker runner..."
e2e_ssh_peer "$VM_IP" "$PEER_VM_PRIVATE_IP" "$SSH_KEY_PATH" "$VM_ADMIN" \
	"sudo kill -TERM '${RUNNER2_PID}' >/dev/null 2>&1 || true"
