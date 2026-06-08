#!/usr/bin/env bash
# Provisions an Azure VM, runs the Firecracker e2e lane on it, and always
# destroys the VM resources afterwards. Passes all arguments through to
# e2e/run-firecracker.sh on the VM.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
OUTPUT_FILE="$(mktemp)"
VM_IP=""
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

echo "==> Provisioning Firecracker e2e VM..."
GITHUB_OUTPUT="$OUTPUT_FILE" bash e2e/infra/scripts/provision-firecracker-e2e-vm.sh

while IFS='=' read -r key value; do
	case "$key" in
	vm_ip) VM_IP="$value" ;;
	ssh_key_path) SSH_KEY_PATH="$value" ;;
	esac
done <"$OUTPUT_FILE"

if [[ -z "$VM_IP" || -z "$SSH_KEY_PATH" ]]; then
	echo "ERROR: provision script did not output vm_ip and ssh_key_path" >&2
	exit 1
fi

echo "==> Running Firecracker e2e tests on ${VM_IP}..."
SSH_OPTS="-o StrictHostKeyChecking=no -o ServerAliveInterval=30 -o ServerAliveCountMax=6"
TESTS_STARTED=1
ssh $SSH_OPTS -i "$SSH_KEY_PATH" "azureuser@${VM_IP}" bash -s -- "$@" <<'EOF'
set -euxo pipefail
export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH

cd ~/project
bash e2e/run-firecracker.sh "$@"
EOF
