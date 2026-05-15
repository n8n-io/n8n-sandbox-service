#!/usr/bin/env bash
# Destroys the Azure VM and all associated resources provisioned by
# provision-e2e-vm.sh. Uses Terraform state and auto.tfvars from e2e/infra/.
set -euo pipefail

cd "$(dirname "$0")/../../.."

TF_DIR="e2e/infra"

if [[ ! -f "${TF_DIR}/terraform.tfstate" ]]; then
	echo "No Terraform state found — nothing to destroy."
	exit 0
fi

echo "==> Destroying Azure VM resources via Terraform..."
terraform -chdir="$TF_DIR" destroy -auto-approve -input=false

echo "==> Cleanup complete"
