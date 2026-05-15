#!/usr/bin/env bash
# Destroys the Azure VM and all associated resources provisioned by
# provision-e2e-vm.sh. Uses Terraform state from e2e/infra/.
set -euo pipefail

cd "$(dirname "$0")/../../.."

: "${RESOURCE_GROUP:?RESOURCE_GROUP is required}"

TF_DIR="e2e/infra"
VM_NAME="e2e-sysbox-1778838174"
# VM_NAME="e2e-sysbox-$(date +%s)"
SSH_KEY_PATH="$HOME/.ssh/e2e-vm-key"


if [[ ! -f "${TF_DIR}/terraform.tfstate" ]]; then
	echo "No Terraform state found — nothing to destroy."
	exit 0
fi

echo "==> Destroying Azure VM resources via Terraform..."
terraform -chdir="$TF_DIR" destroy -auto-approve -input=false \
	-var "resource_group_name=$RESOURCE_GROUP" \
	-var "vm_name=$VM_NAME" \
	-var "ssh_public_key_path=${SSH_KEY_PATH}.pub"

echo "==> Cleanup complete"
