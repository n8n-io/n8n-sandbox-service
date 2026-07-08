#!/usr/bin/env bash
# Provisions an Azure VM for Firecracker e2e tests using Terraform, transfers
# the project source, and builds Firecracker host dependencies plus test assets.
# Requires: RESOURCE_GROUP env var.
# Outputs: vm_name, vm_ip, ssh_key_path (to $GITHUB_OUTPUT when running in CI).
set -euo pipefail

cd "$(dirname "$0")/../../.."
# shellcheck source=e2e/lib/common.sh
source e2e/lib/common.sh

: "${RESOURCE_GROUP:?RESOURCE_GROUP is required}"

TF_DIR="e2e/infra"
VM_NAME="e2e-firecracker-${GITHUB_RUN_ID:-$(date +%s)}"
LOCATION="${FIRECRACKER_E2E_LOCATION:-germanywestcentral}"
VM_SIZE="${FIRECRACKER_E2E_VM_SIZE:-Standard_D4s_v3}"
OS_DISK_SIZE_GB="${FIRECRACKER_E2E_OS_DISK_SIZE_GB:-80}"
SSH_KEY_PATH="$HOME/.ssh/e2e-firecracker-vm-key"
VM_ADMIN="azureuser"

output() {
	echo "$1=$2"
	if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
		echo "$1=$2" >> "$GITHUB_OUTPUT"
	fi
}

mkdir -p "$(dirname "$SSH_KEY_PATH")"
if [[ ! -f "$SSH_KEY_PATH" ]]; then
	echo "==> Generating ephemeral SSH keypair..."
	ssh-keygen -t ed25519 -f "$SSH_KEY_PATH" -N "" -q
else
	echo "==> Using existing SSH keypair at ${SSH_KEY_PATH}"
fi

peer_vm_tf=false
case "${E2E_PEER_VM_ENABLED:-false}" in
1 | true | yes) peer_vm_tf=true ;;
esac

cat > "${TF_DIR}/e2e-vm.auto.tfvars.json" <<EOF
{
  "resource_group_name": "$RESOURCE_GROUP",
  "vm_name": "$VM_NAME",
  "location": "$LOCATION",
  "vm_size": "$VM_SIZE",
  "os_disk_size_gb": $OS_DISK_SIZE_GB,
  "ssh_public_key_path": "${SSH_KEY_PATH}.pub",
  "peer_vm_enabled": ${peer_vm_tf}
}
EOF

echo "==> Provisioning Azure VM via Terraform..."
terraform -chdir="$TF_DIR" init -input=false
terraform -chdir="$TF_DIR" apply -auto-approve -input=false

VM_IP=$(terraform -chdir="$TF_DIR" output -raw vm_public_ip)
VM_PRIVATE_IP=$(terraform -chdir="$TF_DIR" output -raw vm_private_ip)
PEER_VM_PUBLIC_IP=""
PEER_VM_PRIVATE_IP=""
if [[ "$peer_vm_tf" == true ]]; then
	PEER_VM_PUBLIC_IP=$(terraform -chdir="$TF_DIR" output -raw peer_vm_public_ip)
	PEER_VM_PRIVATE_IP=$(terraform -chdir="$TF_DIR" output -raw peer_vm_private_ip)
fi

REPO_TGZ="$(mktemp /tmp/n8n-sandbox-repo.XXXXXX.tar.gz)"
REMOTE_ENV="$(e2e_firecracker_setup_remote_env)"
trap 'rm -f "$REPO_TGZ"' EXIT

echo "==> Packing repository tarball..."
e2e_pack_repo_tarball "$REPO_TGZ" "$(pwd)"

setup_control_pid=""
setup_peer_pid=""
e2e_setup_firecracker_host control "$SSH_KEY_PATH" "$VM_ADMIN" "$VM_IP" "" "$REPO_TGZ" "$REMOTE_ENV" &
setup_control_pid=$!

if [[ "$peer_vm_tf" == true ]]; then
	e2e_setup_firecracker_host peer "$SSH_KEY_PATH" "$VM_ADMIN" "$PEER_VM_PUBLIC_IP" "" "$REPO_TGZ" "$REMOTE_ENV" &
	setup_peer_pid=$!
fi

setup_failed=0
wait "$setup_control_pid" || setup_failed=1
if [[ -n "$setup_peer_pid" ]]; then
	wait "$setup_peer_pid" || setup_failed=1
fi
if [[ "$setup_failed" -ne 0 ]]; then
	echo "==> One or more Firecracker host setups failed." >&2
	exit 1
fi

output "vm_name" "$VM_NAME"
output "vm_ip" "$VM_IP"
output "vm_private_ip" "$VM_PRIVATE_IP"
output "ssh_key_path" "$SSH_KEY_PATH"
if [[ "$peer_vm_tf" == true ]]; then
	output "peer_vm_public_ip" "$PEER_VM_PUBLIC_IP"
	output "peer_vm_private_ip" "$PEER_VM_PRIVATE_IP"
fi

echo "==> Firecracker VM ${VM_NAME} is ready at ${VM_IP}"
if [[ "$peer_vm_tf" == true ]]; then
	echo "==> Firecracker peer VM is ready at ${PEER_VM_PUBLIC_IP} (private ${PEER_VM_PRIVATE_IP})"
fi
