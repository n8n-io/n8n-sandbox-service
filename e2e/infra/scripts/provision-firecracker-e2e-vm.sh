#!/usr/bin/env bash
# Provisions an Azure VM for Firecracker e2e tests using Terraform, transfers
# the project source, and builds Firecracker host dependencies plus test assets.
# Requires: RESOURCE_GROUP env var.
# Outputs: vm_name, vm_ip, ssh_key_path (to $GITHUB_OUTPUT when running in CI).
set -euo pipefail

cd "$(dirname "$0")/../../.."

: "${RESOURCE_GROUP:?RESOURCE_GROUP is required}"

TF_DIR="e2e/infra"
VM_NAME="e2e-firecracker-${GITHUB_RUN_ID:-$(date +%s)}"
LOCATION="${FIRECRACKER_E2E_LOCATION:-germanywestcentral}"
VM_SIZE="${FIRECRACKER_E2E_VM_SIZE:-Standard_D4s_v3}"
OS_DISK_SIZE_GB="${FIRECRACKER_E2E_OS_DISK_SIZE_GB:-80}"
SSH_KEY_PATH="$HOME/.ssh/e2e-firecracker-vm-key"
VM_ADMIN="azureuser"
SSH_OPTS="-o StrictHostKeyChecking=no -o ServerAliveInterval=30 -o ServerAliveCountMax=6"

shell_quote() {
	printf "%q" "$1"
}

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

cat > "${TF_DIR}/e2e-vm.auto.tfvars.json" <<EOF
{
  "resource_group_name": "$RESOURCE_GROUP",
  "vm_name": "$VM_NAME",
  "location": "$LOCATION",
  "vm_size": "$VM_SIZE",
  "os_disk_size_gb": $OS_DISK_SIZE_GB,
  "ssh_public_key_path": "${SSH_KEY_PATH}.pub"
}
EOF

echo "==> Provisioning Azure VM via Terraform..."
terraform -chdir="$TF_DIR" init -input=false
terraform -chdir="$TF_DIR" apply -auto-approve -input=false

VM_IP=$(terraform -chdir="$TF_DIR" output -raw vm_public_ip)

echo "==> Waiting for SSH on ${VM_IP}..."
for i in $(seq 1 30); do
	if ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 \
		-i "$SSH_KEY_PATH" "${VM_ADMIN}@${VM_IP}" "echo ready" 2>/dev/null; then
		echo "SSH is available"
		break
	fi
	if [[ "$i" -eq 30 ]]; then
		echo "SSH connection failed after 5 minutes"
		exit 1
	fi
	echo "Waiting... ($i/30)"
	sleep 3
done

echo "==> Transferring code to VM..."
GNUTAR=$(command -v gtar || command -v tar)
COPYFILE_DISABLE=1 "$GNUTAR" czf /tmp/repo.tar.gz \
	--no-xattrs \
	--exclude=.git \
	--exclude='.DS_Store' \
	--exclude='._*' \
	--exclude=bin \
	--exclude=dist \
	--exclude=node_modules \
	--exclude='e2e/infra/.terraform' \
	--exclude='e2e/infra/*.tfstate*' \
	-C "$(pwd)" .
scp $SSH_OPTS -i "$SSH_KEY_PATH" /tmp/repo.tar.gz "${VM_ADMIN}@${VM_IP}:/tmp/repo.tar.gz"
ssh $SSH_OPTS -i "$SSH_KEY_PATH" "${VM_ADMIN}@${VM_IP}" \
	"mkdir -p ~/project && tar xzf /tmp/repo.tar.gz -C ~/project && rm /tmp/repo.tar.gz"
rm -f /tmp/repo.tar.gz

echo "==> Setting up Firecracker VM..."
REMOTE_ENV=""
if [[ -n "${FIRECRACKER_VERSION:-}" ]]; then
	REMOTE_ENV+=" FIRECRACKER_VERSION=$(shell_quote "$FIRECRACKER_VERSION")"
fi
if [[ -n "${FIRECRACKER_TARBALL_SHA256:-}" ]]; then
	REMOTE_ENV+=" FIRECRACKER_TARBALL_SHA256=$(shell_quote "$FIRECRACKER_TARBALL_SHA256")"
fi
if [[ -n "${FIRECRACKER_CI_VERSION:-}" ]]; then
	REMOTE_ENV+=" FIRECRACKER_CI_VERSION=$(shell_quote "$FIRECRACKER_CI_VERSION")"
fi
if [[ -n "${FIRECRACKER_E2E_ROOTFS_SIZE_MB:-}" ]]; then
	REMOTE_ENV+=" FIRECRACKER_E2E_ROOTFS_SIZE_MB=$(shell_quote "$FIRECRACKER_E2E_ROOTFS_SIZE_MB")"
fi
ssh $SSH_OPTS -i "$SSH_KEY_PATH" "${VM_ADMIN}@${VM_IP}" \
	"${REMOTE_ENV:+${REMOTE_ENV} }bash ~/project/e2e/infra/scripts/setup-firecracker-e2e-vm.sh"

output "vm_name" "$VM_NAME"
output "vm_ip" "$VM_IP"
output "ssh_key_path" "$SSH_KEY_PATH"

echo "==> Firecracker VM ${VM_NAME} is ready at ${VM_IP}"
