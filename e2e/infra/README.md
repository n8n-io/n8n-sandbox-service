# E2E VM (Azure)

Terraform config that provisions an ephemeral Ubuntu 24.04 VM in Azure for running e2e tests with sysbox. Creates: VNet, subnet, NSG (SSH-only), public IP, NIC, and VM.

In CI, the Sysbox workflow uses `e2e/infra/scripts/provision-e2e-vm.sh` and
`e2e/infra/scripts/cleanup-e2e-vm.sh` to manage the Azure VM. That workflow runs
on manual dispatch and on PRs labeled `e2e-sysbox`.

The same Terraform VM shape is also reused by the Firecracker e2e lane via
`e2e/infra/scripts/provision-firecracker-e2e-vm.sh`. That path uses a
Firecracker-specific setup script, runs a nested KVM capability preflight,
installs Firecracker/jailer, builds the kernel/rootfs template locally from
pinned upstream inputs, and creates the snapshot on the VM. The Firecracker
workflow runs on manual dispatch and on PRs labeled `e2e-firecracker`.

## Prerequisites

- [Terraform](https://developer.hashicorp.com/terraform/install) >= 1.0
- Azure CLI authenticated (`az login`)
- An existing Azure resource group

## Usage

For the Firecracker lane, run the full provision/test/cleanup flow from the
repository root:

```bash
RESOURCE_GROUP=my-resource-group bash e2e/run-firecracker-azure.sh
```

Any extra arguments are passed to `e2e/run-firecracker.sh` on the VM:

```bash
RESOURCE_GROUP=my-resource-group \
  bash e2e/run-firecracker-azure.sh e2e/tests/sandbox-api.spec.ts
```

The wrapper collects logs on failure and destroys the Azure VM resources on
exit.

## Firecracker assets

Firecracker e2e provisioning allows overriding:

- `FIRECRACKER_E2E_LOCATION`: optional Azure region override.
- `FIRECRACKER_E2E_VM_SIZE`: optional VM size override; Firecracker runs default
  to `Standard_D4s_v3`.
- `FIRECRACKER_VERSION`: optional Firecracker release override; defaults to
  `v1.14.1`.
- `FIRECRACKER_TARBALL_SHA256`: required when overriding to a Firecracker
  release that is not checksum-pinned by the setup script.
- `FIRECRACKER_CI_VERSION`: optional Firecracker CI asset line override;
  defaults to the configured Firecracker release line, for example `v1.14`.
- `FIRECRACKER_E2E_ROOTFS_SIZE_MB`: optional ext4 rootfs size; defaults to
  `1024`.
- `FIRECRACKER_E2E_SNAPSHOT_MEM_MIB`: optional guest memory for local snapshot
  generation; defaults to `512`.
- `FIRECRACKER_E2E_SNAPSHOT_VCPUS`: optional vCPU count for local snapshot
  generation; defaults to `1`.

The Firecracker e2e setup builds the bootable template locally on the VM:

1. Install the configured Firecracker release and jailer.
2. Download the matching Firecracker CI `vmlinux` and Ubuntu squashfs.
3. Convert the squashfs to `/srv/firecracker/template/rootfs.ext4`.
4. Build the n8n sandbox daemon locally and inject it into the rootfs.
5. Boot the VM locally and write `snapshots/mem` and `snapshots/state`.

The e2e asset contract is the configured Firecracker release, the matching
Firecracker CI kernel/rootfs inputs, and the locally built sandbox daemon. The
e2e VM generates the bootable template and snapshot from those pinned inputs
locally.

The setup writes `/srv/firecracker/manifest.json` for diagnostics:

```json
{
  "firecracker_version": "1.14.1",
  "firecracker_ci_version": "v1.14",
  "azure_vm_size": "Standard_D4as_v5",
  "cpu_model": "Intel(R) Xeon(R) Platinum ...",
  "cpu_flags": "...",
  "kvm_tsc_scaling": "1",
  "kernel_file_type": "ELF 64-bit LSB executable, x86-64, ...",
  "rootfs_file_type": "Linux rev 1.0 ext4 filesystem data, ..."
}
```
