#!/usr/bin/env bash
# Provisions an Azure VM (and optional peer runner VM), runs the Firecracker e2e
# lane, and always destroys the VM resources afterwards.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
OUTPUT_FILE="$(mktemp)"
VM_IP=""
VM_PRIVATE_IP=""
PEER_VM_PUBLIC_IP=""
PEER_VM_PRIVATE_IP=""
SSH_KEY_PATH=""
EXIT_CODE=0
TESTS_STARTED=0

cleanup() {
	EXIT_CODE=$?
	trap - EXIT
	local cleanup_exit=0

	if [[ "$EXIT_CODE" -ne 0 && -n "$VM_IP" && -n "$SSH_KEY_PATH" ]]; then
		echo "==> Collecting Firecracker e2e logs before cleanup..."
		VM_IP="$VM_IP" SSH_KEY_PATH="$SSH_KEY_PATH" \
			bash "$PROJECT_DIR/e2e/infra/scripts/collect-firecracker-e2e-vm-logs.sh" || true
	fi

	echo "==> Cleaning up Azure VM resources..."
	bash "$PROJECT_DIR/e2e/infra/scripts/cleanup-e2e-vm.sh" || cleanup_exit=$?
	rm -f "$OUTPUT_FILE"

	if [[ "$cleanup_exit" -ne 0 ]]; then
		echo "==> Azure VM cleanup failed with exit code ${cleanup_exit}." >&2
	fi

	if [[ "$TESTS_STARTED" == "1" && "$EXIT_CODE" -eq 0 ]]; then
		echo "==> Firecracker e2e flow finished: tests passed."
	elif [[ "$TESTS_STARTED" == "1" ]]; then
		echo "==> Firecracker e2e flow finished: tests failed with exit code ${EXIT_CODE}." >&2
	else
		echo "==> Firecracker e2e flow finished: tests did not complete; exit code ${EXIT_CODE}." >&2
	fi
	exit "$EXIT_CODE"
}
trap cleanup EXIT

cd "$PROJECT_DIR"

echo "==> Provisioning Firecracker e2e VMs (control + peer runner)..."
E2E_PEER_VM_ENABLED=true GITHUB_OUTPUT="$OUTPUT_FILE" \
	bash e2e/infra/scripts/provision-firecracker-e2e-vm.sh

while IFS='=' read -r key value; do
	case "$key" in
	vm_ip) VM_IP="$value" ;;
	vm_private_ip) VM_PRIVATE_IP="$value" ;;
	peer_vm_public_ip) PEER_VM_PUBLIC_IP="$value" ;;
	peer_vm_private_ip) PEER_VM_PRIVATE_IP="$value" ;;
	ssh_key_path) SSH_KEY_PATH="$value" ;;
	esac
done <"$OUTPUT_FILE"

if [[ -z "$VM_IP" || -z "$VM_PRIVATE_IP" || -z "$PEER_VM_PRIVATE_IP" || -z "$SSH_KEY_PATH" ]]; then
	echo "ERROR: provision script did not output control and peer VM addresses" >&2
	exit 1
fi

echo "==> Running Firecracker e2e tests on control VM ${VM_IP}..."
SSH_OPTS="-o StrictHostKeyChecking=no -o ServerAliveInterval=30 -o ServerAliveCountMax=6"
TESTS_STARTED=1
ssh $SSH_OPTS -i "$SSH_KEY_PATH" "azureuser@${VM_IP}" bash -s -- "$@" <<'EOF'
set -euxo pipefail
export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH

cd ~/project
bash e2e/run-firecracker.sh "$@"
if [[ $# -eq 0 ]]; then
	sleep 2
	bash e2e/run-firecracker-idle-ttl.sh
fi
EOF

if [[ $# -eq 0 ]]; then
	sleep 2
	VM_IP="$VM_IP" \
		VM_PRIVATE_IP="$VM_PRIVATE_IP" \
		PEER_VM_PRIVATE_IP="$PEER_VM_PRIVATE_IP" \
		SSH_KEY_PATH="$SSH_KEY_PATH" \
		bash e2e/run-firecracker-two-runners-azure.sh
fi
