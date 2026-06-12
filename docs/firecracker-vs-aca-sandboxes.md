# Firecracker VMSS vs Azure Container Apps Sandboxes

Short comparison for choosing between self-hosted Firecracker on Azure VMSS and [Azure Container Apps Sandboxes](https://learn.microsoft.com/en-us/azure/container-apps/sandboxes-overview) (preview).

## Headline cost comparison

Shared assumptions:

- Sandbox size: **0.5 vCPU, 1 GiB** (ACA tier **S**, close to our 512 MiB guest)
- Region-class pricing: West Europe / Germany West Central ballpark
- Firecracker: **2× `Standard_D4s_v3`** runners, **4 sandboxes per VM** when steady
- ACA: [consumption per-second pricing](https://azure.microsoft.com/en-us/pricing/details/container-apps/) (~$0.054/active sandbox-hour at tier S)

| Workload pattern | ACA Sandboxes | Firecracker VMSS (2 runners) |
|------------------|---------------|------------------------------|
| **Bursty** — 500 sandbox-hours/month (e.g. 10k sessions × 3 min) | **~$27/mo** | **~$280/mo** |
| **Steady** — 8 sandboxes × 720 h/mo = 5,760 sandbox-hours | **~$311/mo** | **~$280/mo** |

ACA wins on bursty/low-utilization workloads. Self-hosted VMSS wins when runners stay full. Real crossover depends on concurrency, idle-TTL, and VMSS autoscale minimum instance count.

### Calculation footnote

**ACA tier S active rate** (after monthly free grant):

`3600 s × (0.5 × $0.000024 vCPU + 1 × $0.000003 GiB) ≈ $0.054 / sandbox-hour`

- Bursty: `500 h × $0.054 ≈ $27`
- Steady: `8 × 720 h × $0.054 ≈ $311`
- Suspended/idle ACA pricing is not included; suspend may reduce bursty cost further.

**Firecracker VMSS**:

- `Standard_D4s_v3` ≈ **$0.192/h** (~$140/VM/month) × 2 VMs ≈ **$280/month** fixed
- Steady: `$280 ÷ 5,760 sandbox-hours ≈ $0.049 / sandbox-hour` (ignoring engineering/ops)
- Bursty: same **$280** if minimum capacity is 2 VMs regardless of session count

Replace list prices with your Azure agreement / EUR rates before publishing externally.

## Comparison

| Dimension | Self-hosted Firecracker | ACA Sandboxes (preview) |
|-----------|-------------------------|-------------------------|
| **Monthly cost (bursty)** | ~$280/mo (2 VMs, 24/7) | ~$27/mo (500 sandbox-h) |
| **Monthly cost (steady)** | ~$280/mo | ~$311/mo (8 always-on sandboxes) |
| **Security** | MicroVM + jailer + per-sandbox netns; we own egress policy | Hardware-isolated microVM; [built-in egress policies](https://learn.microsoft.com/en-us/azure/container-apps/sandboxes-egress-policies) |
| **Flexibility** | Kernel, rootfs, daemon, block devices | OCI image + platform lifecycle APIs |
| **Control** | Runner owns slots, snapshots, networking, cleanup | Platform owns host fleet, suspend/resume, pooling |
| **Cross-cloud** | KVM hosts anywhere | Azure-only (`Microsoft.App/SandboxGroups`) |
| **Operate / build** | High: VMSS, assets, CPU placement, snapshot portability | Low: managed resource + SDK/API |
| **Technology** | Mature Firecracker; Azure VMSS snapshot portability under study | Preview (2026); same primitive Microsoft uses internally |
| **State** | Full memory snapshots (CPU-sensitive); see [snapshot CPU notes](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/versioning.md#cpu-model) | Built-in suspend/resume; snapshots billed as Blob after preview |
| **Maturity** | E2e lane working; production VMSS TBD | Public preview; APIs evolving |
| **Tenant workspace** | Blob + per-sandbox ext4 hydration (planned) | Platform volumes/state (needs spike vs our file API) |

## Links

- [ACA Sandboxes overview](https://learn.microsoft.com/en-us/azure/container-apps/sandboxes-overview)
- [ACA Sandboxes announcement](https://techcommunity.microsoft.com/blog/appsonazureblog/introducing-azure-container-apps-sandboxes-secure-infrastructure-for-agentic-wor/4524131)
- [ACA pricing](https://azure.microsoft.com/en-us/pricing/details/container-apps/)
- [Firecracker snapshot CPU model](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/versioning.md#cpu-model)
- [Firecracker CPU templates](https://github.com/firecracker-microvm/firecracker/blob/main/docs/cpu_templates/cpu-templates.md)
- [cpu-template-helper](https://github.com/firecracker-microvm/firecracker/blob/main/docs/cpu_templates/cpu-template-helper.md)
- [Firecracker snapshot portability](firecracker-intel-snapshot-compat-report.md)
- [Firecracker runner README](../internal/runner/runtime/firecracker.ee/README.md)
- [Snapshot compatibility study](../scripts/firecracker-snapshot-compat/README.md)
