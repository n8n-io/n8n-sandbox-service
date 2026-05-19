#!/usr/bin/env bash
# Provisions an Azure VM for e2e tests using Terraform, transfers the project
# source, and installs all dependencies (Docker, sysbox, Go, Node, pnpm).
# Requires: RESOURCE_GROUP env var, authenticated Azure CLI or ARM_* env vars.
# Outputs: vm_name, vm_ip, ssh_key_path (to $GITHUB_OUTPUT when running in CI).
set -euo pipefail

cd "$(dirname "$0")/../../.."

: "${RESOURCE_GROUP:?RESOURCE_GROUP is required}"

TF_DIR="e2e/infra"
VM_NAME="e2e-sysbox-${GITHUB_RUN_ID:-$(date +%s)}"
SSH_KEY_PATH="$HOME/.ssh/e2e-vm-key"
VM_ADMIN="azureuser"
SSH_OPTS="-o StrictHostKeyChecking=no -o ServerAliveInterval=30 -o ServerAliveCountMax=6"

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
"$GNUTAR" czf /tmp/repo.tar.gz \
	--no-xattrs \
	--exclude=.git \
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

echo "==> Setting up VM (Docker, sysbox, Go, Node, pnpm)..."
ssh $SSH_OPTS -i "$SSH_KEY_PATH" "${VM_ADMIN}@${VM_IP}" "bash ~/project/e2e/infra/scripts/setup-e2e-vm.sh"

output "vm_name" "$VM_NAME"
output "vm_ip" "$VM_IP"
output "ssh_key_path" "$SSH_KEY_PATH"

echo "==> VM ${VM_NAME} is ready at ${VM_IP}"
