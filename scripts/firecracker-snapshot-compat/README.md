# Firecracker snapshot compatibility study

Throwaway tooling to measure Firecracker snapshot restore compatibility across Azure VMs in the same SKU and CPU template mitigations.

## Goal

Answer whether snapshots created on one VM restore on others in the same SKU, and whether CPU templates (`T2`, `C3`, `T2S`, `T2CL`, `cpu-template-helper` custom, `no-xcrs`) improve portability.

## Prerequisites

- Azure CLI + Terraform
- `RESOURCE_GROUP` env var
- SSH access to cluster VMs

## Quick start

```bash
export RESOURCE_GROUP=my-resource-group

# 1. Provision VMs, copy repo, setup Firecracker on each host
bash scripts/firecracker-snapshot-compat/infra/provision-vmss.sh

# 2. Run full template matrix (creates on each distinct CPU, restores on all)
bash scripts/firecracker-snapshot-compat/run-matrix.sh

# 3. Summarize (defaults to the current study from instances.json)
bash scripts/firecracker-snapshot-compat/summarize-results.sh

# 4. Cleanup
bash scripts/firecracker-snapshot-compat/infra/destroy-vmss.sh

# Legacy stack or explicit deployment name (prompts before deleting):
RESOURCE_GROUP=spokes-gwc DEPLOYMENT_NAME=fc-snap-compat-1781245099 \
  bash scripts/firecracker-snapshot-compat/infra/destroy-deployment.sh

# Non-interactive (e.g. CI):
bash scripts/firecracker-snapshot-compat/infra/destroy-deployment.sh --force
```

## Cluster layout

- SKU: `Standard_D4s_v3` (override with `COMPAT_VM_SIZE`)
- Region: `germanywestcentral` (override with `COMPAT_LOCATION`)
- Instances: 3 (override with `COMPAT_INSTANCE_COUNT`)
- Resume a failed provision: set `COMPAT_DEPLOYMENT_NAME` to the existing deployment name
- One VM per instance, each with its own public IP and SSH on port 22 (no load balancer)

Terraform lives in [`infra/`](infra/). Instance metadata is written to `instances.json` after provision.

## Template variants (phase 1)

| Variant | Mechanism |
|---------|-----------|
| `none` | No CPU template |
| `T2` | `"cpu_template": "T2"` on `/machine-config` (Intel Skylake/Cascade Lake/Ice Lake baseline) |
| `C3` | `"cpu_template": "C3"` on `/machine-config` (Intel Skylake-class, most restrictive static template) |
| `T2S` | `"cpu_template": "T2S"` on `/machine-config` (newer → older Intel migration) |
| `T2CL` | `"cpu_template": "T2CL"` on `/machine-config` (Cascade Lake / Ice Lake) |
| `helper-custom` | Custom `/cpu-config` from **all** host fingerprints (`cpu-template-helper static`) |
| `helper-intel-only` | Custom `/cpu-config` from **Intel-only** fingerprints — one representative per CPU signature (excludes AMD) |
| `no-xcrs` | Custom `/cpu-config` with `"kvm_capabilities": ["!56"]` |

Templates are applied at snapshot **create** (`/machine-config` or `/cpu-config`). **Restore** calls `/snapshot/load` only (matching production); custom variants (`helper-custom`, `helper-intel-only`, `no-xcrs`) may set `/cpu-config` before load. Static templates (`T2`, `C3`, `T2S`, `T2CL`) are embedded in the snapshot from create.

After heterogeneous provision, `sync-cpu-configs.sh` rebuilds `helper-intel-only` from the merged `instances.json` and copies configs to every VM.

## Scripts

| Script | Purpose |
|--------|---------|
| `infra/provision-vmss.sh` | Terraform apply, setup all VMs (name kept for compatibility) |
| `infra/destroy-vmss.sh` | Delete deployment (Azure CLI; works without matching Terraform state) |
| `infra/destroy-deployment.sh` | Same, with explicit `--deployment` / `--resource-group` |
| `collect-host-fingerprint.sh` | Host CPU/KVM fingerprint JSON |
| `create-snapshot.sh` | Create snapshot with template variant |
| `restore-snapshot.sh` | Load snapshot and verify `/healthz` |
| `build-helper-custom-config.sh` | Build custom CPU config from fingerprints (supports `--intel-only`, `--representatives`) |
| `build-helper-intel-config.sh` | Intel-only representative fingerprints → custom CPU config |
| `sync-cpu-configs.sh` | Rebuild helper configs for the full study and copy to all VMs |
| `provision-heterogeneous.sh` | Provision 3 cohorts for CPU heterogeneity |
| `analyze-fingerprints.sh` | Annotate `instances.json` and write study `run-manifest.json` |
| `run-matrix.sh` | Run all (variant × create × restore) combinations |
| `summarize-results.sh` | Markdown table for a study directory (default: current study) |
| `infra/destroy-heterogeneous.sh` | Tear down all cohorts from `deployments.json` |

## Output

- `instances.json` — live VM names, public IPs, and `cpu_analysis` (always reflects latest provision)
- `fingerprints/` — live per-instance host fingerprints (per cohort subdirectory)
- `results/<study_id>/` — self-contained output per study:
  - `run-manifest.json` — CPU analysis and instance metadata at provision time
  - `instances.json`, `deployments.json`, `fingerprints/` — study snapshot
  - `results.jsonl` — matrix results (one row per cell; denormalized CPU fields)
  - `matrix-plan.json`, `matrix-plan.tsv` — planned creates/restores
  - `summary.md` — generated summary table (`pass*` marks same-CPU cross-host cells)
- `results/latest` — symlink to the most recently provisioned study

## Matrix modes

| Mode | Env | Behavior |
|------|-----|----------|
| `smart` (default) | `COMPAT_MATRIX_MODE=smart` | One create per CPU signature per variant; same-host restore on creator; cross-host restore only between **different** CPU signatures (one representative VM per signature) |
| `full` | `COMPAT_MATRIX_MODE=full` | Every instance creates; every snapshot restored on every instance |

Set `COMPAT_MATRIX_CREATE_ALL=1` to create on every instance while keeping smart restore filtering.

`run-matrix.sh` prints per-step progress (`N/M`, percent, elapsed, ETA) across creates and restores.

Each study writes to its own directory under `results/<study_id>/`. Re-runs with the same study skip steps already recorded (`COMPAT_MATRIX_RESUME=1`, default). Use `COMPAT_MATRIX_RESET=1` for a fresh `results.jsonl` in that study dir and wipe remote snapshot dirs. A new provision gets a new `study_id` and a new results folder; older studies are untouched.

Migrate legacy flat `results/results.jsonl` layout:

```bash
node scripts/firecracker-snapshot-compat/lib/migrate-results-layout.js scripts/firecracker-snapshot-compat
```

`run-matrix.sh` classifies failures: **portability** (expected CPU cross-host restore failures), **snapshot** (truncated/invalid snapshot at create — loud banner; same-host snapshot fails abort after two consecutive), **systemic** (script/infra bugs), **other** (warn). Post-create checks verify snapshot file sizes. Set `COMPAT_FAIL_FAST_SYSTEMIC=0` to disable early abort. Exit code is non-zero for snapshot/systemic/unexpected failures, not for portability `fail` rows.

Example for 9 VMs with 3 distinct CPUs and 8 variants:

- **full**: 72 creates + 648 restores
- **smart**: 24 creates + 72 restores (24 same-host + 48 cross-signature)

Preview the plan:

```bash
node scripts/firecracker-snapshot-compat/lib/matrix-plan.js scripts/firecracker-snapshot-compat/instances.json smart
```

## CPU homogeneity

If all VMs share the same guest-visible CPU signature, cross-instance restore passes are tagged
`cross_host_portability_test: false` in `results.jsonl` and shown as `pass*` in `summary.md`.
Those cells are informational only.

Production guidance: [Firecracker snapshot portability report](../../docs/firecracker-intel-snapshot-compat-report.md).

Re-run annotation after provision:

```bash
bash scripts/firecracker-snapshot-compat/analyze-fingerprints.sh
```

## Heterogeneous study (phase 2)

Provision three **Intel-only** cohorts (3 VMs each by default) — different D-series generations to surface CPU stepping differences without AMD:

| Cohort | Region | SKU | Intel generation |
|--------|--------|-----|------------------|
| `gwc-d4sv3` | germanywestcentral | Standard_D4s_v3 | 3rd-gen D |
| `weu-d4sv4` | westeurope | Standard_D4s_v4 | 4th-gen D |
| `gwc-d4sv5` | germanywestcentral | Standard_D4s_v5 | 5th-gen D |

Azure does not guarantee distinct CPU models per SKU; run `analyze-fingerprints.sh` after provision to confirm `distinct_cpu_count`.

```bash
export RESOURCE_GROUP=my-resource-group
bash scripts/firecracker-snapshot-compat/provision-heterogeneous.sh
bash scripts/firecracker-snapshot-compat/run-matrix.sh
bash scripts/firecracker-snapshot-compat/infra/destroy-heterogeneous.sh
```

Override cohort regions/SKUs by editing `provision-heterogeneous.sh`.

## Phase 3 (conditional)

After heterogeneous results — e.g. combine `helper-custom` + `no-xcrs`, or test Dedicated Host placement.

## Related docs

- [Firecracker snapshot portability report](../../docs/firecracker-intel-snapshot-compat-report.md)
- [Firecracker runner README](../../internal/runner/runtime/firecracker.ee/README.md)
- [ACA vs Firecracker comparison](../../docs/firecracker-vs-aca-sandboxes.md)
