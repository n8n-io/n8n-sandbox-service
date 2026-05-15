# E2E VM (Azure)

Terraform config that provisions an ephemeral Ubuntu 24.04 VM in Azure for running e2e tests with sysbox. Creates: VNet, subnet, NSG (SSH-only), public IP, NIC, VM, and an auto-shutdown schedule as a safety net.

In CI this is driven automatically by `e2e/infra/scripts/provision-e2e-vm.sh` and `e2e/infra/scripts/cleanup-e2e-vm.sh`. The instructions below are for running it manually.

## Prerequisites

- [Terraform](https://developer.hashicorp.com/terraform/install) >= 1.0
- Azure CLI authenticated (`az login`)
- An existing Azure resource group

## Usage

```bash
# Generate a temporary SSH key (or use an existing one)
ssh-keygen -t ed25519 -f /tmp/e2e-key -N "" -q

# Init and apply
cd e2e/infra
terraform init
terraform apply \
  -var "resource_group_name=my-resource-group" \
  -var "vm_name=e2e-test-vm" \
  -var "ssh_public_key_path=/tmp/e2e-key.pub" \
  -var "auto_shutdown_time=2000"

# SSH into the VM
VM_IP=$(terraform output -raw vm_public_ip)
ssh -i /tmp/e2e-key azureuser@$VM_IP

# When done, tear everything down
terraform destroy
```

## Variables

| Name | Description | Default |
|------|-------------|---------|
| `resource_group_name` | Existing Azure resource group (required) | — |
| `vm_name` | VM and resource name prefix (required) | — |
| `ssh_public_key_path` | Path to SSH public key file (required) | — |
| `auto_shutdown_time` | Auto-shutdown time in HHMM, UTC (required) | — |
| `vm_size` | Azure VM size | `Standard_D4as_v5` |
| `admin_username` | SSH admin user | `azureuser` |
| `os_disk_size_gb` | OS disk size | `50` |
