# Firecracker snapshot portability on heterogeneous hosts

Empirical study: fc-snap-hetero-1781257524 (2026-06-12) — 4 distinct Intel CPU signatures on Azure VM-scale sets with nested KVM.

Raw data: [`results/fc-snap-hetero-1781257524/`](../scripts/firecracker-snapshot-compat/results/fc-snap-hetero-1781257524/)

## Summary

Firecracker snapshots freeze CPU state (timers, registers, CPUID) as well as memory. On a cloud scale set, hosts are not guaranteed to be CPU-identical — even under the same VM SKU — and Azure will keep rolling out newer hardware over time. You cannot assume that a snapshot taken on one runner will restore on another.

What works in practice: restore on the same CPU fingerprint as the creator, or on a small set of known-compatible hosts (often same generation, sometimes only in one direction). What does not work: treating the fleet as one interchangeable snapshot pool without checks.

Recommendation: do not use CPU templates for a heterogeneous Azure pool. Static templates (T2, C3, T2CL, T2S) did not make cross-host restore reliable and could produce invalid snapshots on some hosts. Custom intersected configs failed at create time. Stay on the host-native CPU model at snapshot time unless you run a homogeneous, validated fleet.

Running in production (snapshot on any host, restore on another):

1. Fingerprint + route — Record the host CPU fingerprint when creating a snapshot; only restore on hosts in a compatible allow-list (same fingerprint first; expand only where testing proves it).
2. Pools by generation — Split runners into snapshot pools per CPU generation; never move snapshots across pools. Re-baseline when Azure introduces new CPUs.
3. Graceful fallback — If restore fails compatibility checks or `/snapshot/load` errors, cold-start from the golden disk image instead of the snapshot (accept slower recovery rather than a broken VM).

## Study fleet

Nine VMs in three cohorts. Azure assigned four distinct guest-visible CPU fingerprints (same marketing name, different fingerprint on D4s_v3 vs D4s_v5 for 8370C).

| Cohort | Region | SKU | Guest CPU | VMs | Matrix creator index |
| --- | --- | --- | --- | --- | --- |
| gwc-d4sv3 | germanywestcentral | Standard_D4s_v3 | Platinum 8370C | 2 | 0 |
| gwc-d4sv3 | germanywestcentral | Standard_D4s_v3 | CPU E5-2673 v4 @ 2.30GHz | 1 | 2 |
| gwc-d4sv5 | germanywestcentral | Standard_D4s_v5 | Platinum 8370C | 3 | 6 |
| weu-d4sv4 | westeurope | Standard_D4s_v4 | Platinum 8272CL | 3 | 3 |


Smart-matrix creators/restorers are one VM per CPU fingerprint (column/row labels in the tables below).

## Restore matrix by CPU template

Each cell: snapshot created on the row CPU, restored on the column CPU. `pass` = guest daemon healthy after restore. `fail` = restore or boot failed. `create_failed` = snapshot could not be created on the row CPU (helper variants only in this run).

### none

| creator \\ restorer | Platinum 8370C (gwc-d4sv3) | CPU E5-2673 v4 @ 2.30GHz (gwc-d4sv3) | Platinum 8272CL (weu-d4sv4) | Platinum 8370C (gwc-d4sv5) |
| --- | --- | --- | --- | --- |
| Platinum 8370C (gwc-d4sv3) | pass | fail | pass | pass |
| CPU E5-2673 v4 @ 2.30GHz (gwc-d4sv3) | fail | pass | fail | pass |
| Platinum 8272CL (weu-d4sv4) | fail | fail | pass | pass |
| Platinum 8370C (gwc-d4sv5) | fail | fail | fail | pass |

Cross-host: 4/12 pass.

### T2

| creator \\ restorer | Platinum 8370C (gwc-d4sv3) | CPU E5-2673 v4 @ 2.30GHz (gwc-d4sv3) | Platinum 8272CL (weu-d4sv4) | Platinum 8370C (gwc-d4sv5) |
| --- | --- | --- | --- | --- |
| Platinum 8370C (gwc-d4sv3) | pass | pass | pass | pass |
| CPU E5-2673 v4 @ 2.30GHz (gwc-d4sv3) | fail | fail | fail | fail |
| Platinum 8272CL (weu-d4sv4) | fail | pass | pass | pass |
| Platinum 8370C (gwc-d4sv5) | fail | fail | fail | pass |

Cross-host: 5/12 pass.

### C3

| creator \\ restorer | Platinum 8370C (gwc-d4sv3) | CPU E5-2673 v4 @ 2.30GHz (gwc-d4sv3) | Platinum 8272CL (weu-d4sv4) | Platinum 8370C (gwc-d4sv5) |
| --- | --- | --- | --- | --- |
| Platinum 8370C (gwc-d4sv3) | pass | pass | pass | pass |
| CPU E5-2673 v4 @ 2.30GHz (gwc-d4sv3) | fail | fail | fail | fail |
| Platinum 8272CL (weu-d4sv4) | fail | pass | pass | pass |
| Platinum 8370C (gwc-d4sv5) | fail | fail | fail | pass |

Cross-host: 5/12 pass.

### T2S

| creator \\ restorer | Platinum 8370C (gwc-d4sv3) | CPU E5-2673 v4 @ 2.30GHz (gwc-d4sv3) | Platinum 8272CL (weu-d4sv4) | Platinum 8370C (gwc-d4sv5) |
| --- | --- | --- | --- | --- |
| Platinum 8370C (gwc-d4sv3) | fail | fail | fail | fail |
| CPU E5-2673 v4 @ 2.30GHz (gwc-d4sv3) | fail | fail | fail | fail |
| Platinum 8272CL (weu-d4sv4) | fail | pass | pass | pass |
| Platinum 8370C (gwc-d4sv5) | fail | fail | fail | fail |

Cross-host: 2/12 pass.

### T2CL

| creator \\ restorer | Platinum 8370C (gwc-d4sv3) | CPU E5-2673 v4 @ 2.30GHz (gwc-d4sv3) | Platinum 8272CL (weu-d4sv4) | Platinum 8370C (gwc-d4sv5) |
| --- | --- | --- | --- | --- |
| Platinum 8370C (gwc-d4sv3) | pass | pass | pass | pass |
| CPU E5-2673 v4 @ 2.30GHz (gwc-d4sv3) | fail | fail | fail | fail |
| Platinum 8272CL (weu-d4sv4) | fail | pass | pass | pass |
| Platinum 8370C (gwc-d4sv5) | fail | fail | fail | pass |

Cross-host: 5/12 pass.

### no-xcrs

| creator \\ restorer | Platinum 8370C (gwc-d4sv3) | CPU E5-2673 v4 @ 2.30GHz (gwc-d4sv3) | Platinum 8272CL (weu-d4sv4) | Platinum 8370C (gwc-d4sv5) |
| --- | --- | --- | --- | --- |
| Platinum 8370C (gwc-d4sv3) | pass | fail | pass | pass |
| CPU E5-2673 v4 @ 2.30GHz (gwc-d4sv3) | fail | pass | fail | pass |
| Platinum 8272CL (weu-d4sv4) | fail | fail | pass | pass |
| Platinum 8370C (gwc-d4sv5) | fail | fail | fail | pass |

Cross-host: 4/12 pass.

### helper-custom

| creator \\ restorer | Platinum 8370C (gwc-d4sv3) | CPU E5-2673 v4 @ 2.30GHz (gwc-d4sv3) | Platinum 8272CL (weu-d4sv4) | Platinum 8370C (gwc-d4sv5) |
| --- | --- | --- | --- | --- |
| Platinum 8370C (gwc-d4sv3) | create_failed | create_failed | create_failed | create_failed |
| CPU E5-2673 v4 @ 2.30GHz (gwc-d4sv3) | create_failed | create_failed | create_failed | create_failed |
| Platinum 8272CL (weu-d4sv4) | create_failed | create_failed | create_failed | create_failed |
| Platinum 8370C (gwc-d4sv5) | create_failed | create_failed | create_failed | create_failed |

Cross-host: 0/12 pass.

### helper-intel-only

| creator \\ restorer | Platinum 8370C (gwc-d4sv3) | CPU E5-2673 v4 @ 2.30GHz (gwc-d4sv3) | Platinum 8272CL (weu-d4sv4) | Platinum 8370C (gwc-d4sv5) |
| --- | --- | --- | --- | --- |
| Platinum 8370C (gwc-d4sv3) | create_failed | create_failed | create_failed | create_failed |
| CPU E5-2673 v4 @ 2.30GHz (gwc-d4sv3) | create_failed | create_failed | create_failed | create_failed |
| Platinum 8272CL (weu-d4sv4) | create_failed | create_failed | create_failed | create_failed |
| Platinum 8370C (gwc-d4sv5) | create_failed | create_failed | create_failed | create_failed |

Cross-host: 0/12 pass.



### Reading the matrix

- Diagonal (same fingerprint): all variants except `T2S` and helper passed; `T2`/`C3`/`T2CL` failed same-host on Broadwell (E5-2673 on D4s_v3).
- Platinum pairs (8370C ↔ 8272CL): mostly pass with `none`, `T2`, `C3`, `T2CL`.
- Any restore onto Broadwell from newer Intel: fail with XCR errors (`Failed to set KVM vcpu xcrs`).
- 8272CL → 8370C D4s_v3: fail with TSC scaling on `none` and `no-xcrs`.
- 8370C D4s_v5 → other fingerprints: fail (TSC or MSR) on `none`; same pattern on static templates.

Representative error messages:

| Pair | Variant | Error |
| --- | --- | --- |
| 8370C D4s_v3 → E5-2673 D4s_v3 | none | Failed to set KVM vcpu xcrs: Invalid argument (os error 22) |
| 8272CL D4s_v4 → 8370C D4s_v3 | none | Could not set TSC scaling within the snapshot: Invalid argument (os error 22) |
| 8370C D4s_v5 → 8370C D4s_v3 | none | Failed to set all KVM MSRs for this vCPU. Only a partial write was done. |
| 8370C D4s_v5 → 8272CL D4s_v4 | none | Failed to set all KVM MSRs for this vCPU. Only a partial write was done. |
| E5-2673 D4s_v3 (same host) | C3 | Failed to get snapshot state from file: Failed to load snapshot state from file: An error occured during bincode decoding: UnexpectedEnd { additional: 8 } |

## T2 and C3 vs Firecracker documentation

[Firecracker’s static template table](https://github.com/firecracker-microvm/firecracker/blob/main/docs/cpu_templates/cpu-templates.md) lists C3 and T2 as intended for Intel Skylake, Cascade Lake, and Ice Lake.

Our fleet covered Ice Lake (8370C), Cascade Lake (8272CL), and an outlier Broadwell (E5-2673 v4 on D4s_v3). Within the documented generations:

- Templates applied at snapshot create and same-host restore worked on 8370C and 8272CL.
- Cross-host restore among 8370C and 8272CL was similar to `none` (templates did not unlock new pairs; TSC-limited directions still failed).

Outside the documented scope:

- Broadwell is not listed for T2/C3. On E5-2673, T2/C3/T2CL produced invalid snapshots at create (same-host restore failed with undecodable `snapshot_state`). That matches “template safe on listed generations” — our drift host is not one of them.

Even on listed generations, templates do not fix snapshot portability limits:

- TSC is saved in the snapshot and is [explicitly excluded from custom template dumps](https://github.com/firecracker-microvm/firecracker/blob/main/docs/cpu_templates/cpu-template-helper.md) (`MSR_IA32_TSC`). Templates shape CPUID at boot; they do not rewrite TSC frequency embedded in an existing snapshot.
- Directional TSC scaling (8272CL → 8370C D4s_v3) failed identically with `none`, T2, C3, and `no-xcrs`.

So: T2/C3 behaved as documented for booting on Skylake-class hosts, but snapshot restore across our mixed fleet was still bounded by TSC, XCR, and the Broadwell outlier — not solved by picking T2 or C3.

## Did `no-xcrs` (`kvm_capabilities: ["!56"]`) fix XCR errors?

No. [`!56`](https://github.com/firecracker-microvm/firecracker/blob/main/docs/cpu_templates/cpu-templates.md) tells Firecracker to run without requiring `KVM_CAP_XCRS` — useful for starting microVMs on hosts that lack XCR support. Our study applied `{"kvm_capabilities":["!56"]}` at both snapshot create and restore (compat tooling only; production uses `/snapshot/load` without `/cpu-config` at restore).

Results vs `none`: 4/12 cross-host pass — identical to none (4/12). Restores onto Broadwell still failed with the same error:

`Failed to set KVM vcpu xcrs: Invalid argument (os error 22)`

Why: `!56` affects whether Firecracker checks for XCR capability at microVM setup. Snapshot restore still replays XCR register state from the snapshot file. Disabling the capability check does not strip XCR state captured on Ice Lake/Cascade Lake creators. `!56` also does not address TSC scaling or partial MSR restore failures seen on other pairs.

## What is TSC (Time Stamp Counter)

The TSC is the CPU’s high-resolution timestamp counter. Guests use it for timing, `rdtsc`, scheduler clocks, and (via `TSC_DEADLINE`) timer interrupts.

Firecracker snapshots save vCPU state including the guest TSC frequency (`tsc_khz`) and related MSRs (`MSR_IA32_TSC`, `MSR_IA32_TSC_DEADLINE`, etc.). On `/snapshot/load`, Firecracker compares the snapshot’s `tsc_khz` to the restore host; if they differ by more than ~250 ppm it calls `KVM_SET_TSC_KHZ` before replaying vCPU state. If that ioctl fails, restore aborts with `Could not set TSC scaling within the snapshot`.

### Why TSC is excluded from CPU templates

Templates shape CPUID (and some MSRs) at **cold boot**. They do not rewrite vCPU state already frozen in a snapshot. `MSR_IA32_TSC` is [explicitly excluded](https://github.com/firecracker-microvm/firecracker/blob/main/docs/cpu_templates/cpu-template-helper.md#msrs-excluded-from-guest-cpu-configuration-dump) from `cpu-template-helper` dumps because it is a live counter tied to guest execution time — not a static “feature bit” you can normalize across hosts. Firecracker still **must** save and restore the TSC MSR blob for snapshots to work; the helper simply cannot safely fabricate a cross-host TSC value at template-build time.

Firecracker’s own comments on restore ordering: `MSR_IA32_TSC_DEADLINE` must be restored **after** `MSR_IA32_TSC`, because KVM reads the guest TSC when writing the deadline MSR ([PR #4666](https://github.com/firecracker-microvm/firecracker/pull/4666), [issue #4099](https://github.com/firecracker-microvm/firecracker/issues/4099)).

### Firecracker TSC references (docs and code)

| Topic | Link |
| --- | --- |
| Snapshot CPU invariants (templates ≠ snapshot portability) | [versioning.md — CPU model](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/versioning.md#cpu-model) |
| `MSR_IA32_TSC` excluded from template dumps | [cpu-template-helper.md appendix](https://github.com/firecracker-microvm/firecracker/blob/main/docs/cpu_templates/cpu-template-helper.md#msrs-excluded-from-guest-cpu-configuration-dump) |
| Persist `tsc_khz` in snapshots + scale on load | [PR #2596](https://github.com/firecracker-microvm/firecracker/pull/2596) |
| Load path: TSC scaling before `restore_state` | [`builder.rs` `build_microvm_from_snapshot`](https://github.com/firecracker-microvm/firecracker/blob/main/src/vmm/src/builder.rs) (~lines 464–476) |
| 250 ppm tolerance, `is_tsc_scaling_required`, `set_tsc_khz` | [`arch/x86_64/vcpu.rs`](https://github.com/firecracker-microvm/firecracker/blob/main/src/vmm/src/arch/x86_64/vcpu.rs) |
| Cross-host TSC tests parked | [issue #2985](https://github.com/firecracker-microvm/firecracker/issues/2985) |
| TSC_DEADLINE restore ordering | [PR #4666](https://github.com/firecracker-microvm/firecracker/pull/4666) |

### KVM vs Firecracker for TSC scaling

Both layers are involved; the failure mode depends on which layer rejects the operation.

| Layer | Role | References |
| --- | --- | --- |
| **Firecracker** | Saves `tsc_khz` in the snapshot; on load, detects frequency mismatch (>250 ppm) and calls `KVM_SET_TSC_KHZ` **before** `KVM_SET_MSRS` / other vCPU restore | [`builder.rs`](https://github.com/firecracker-microvm/firecracker/blob/main/src/vmm/src/builder.rs), [`vcpu.rs` `is_tsc_scaling_required`](https://github.com/firecracker-microvm/firecracker/blob/main/src/vmm/src/arch/x86_64/vcpu.rs) |
| **KVM** | Implements `KVM_SET_TSC_KHZ` / `KVM_GET_TSC_KHZ` (`KVM_CAP_TSC_CONTROL`); decides whether to use TSC multiplier scaling or catch-up mode; returns `-EINVAL` when scaling is impossible | [KVM API §4.55–4.56](https://www.kernel.org/doc/Documentation/virtualization/kvm-api.txt), [`arch/x86/kvm/x86.c` `kvm_set_tsc_khz`](https://github.com/torvalds/linux/blob/v6.6/arch/x86/kvm/x86.c) |

KVM’s `set_tsc_khz` logic (Linux 6.6): if `KVM_CAP_TSC_CONTROL` is absent and the requested guest frequency is **below** host TSC, it logs `user requested TSC rate below hardware speed` and returns `-1`; if above host TSC it falls back to catch-up mode. With TSC control, it computes a multiplier and rejects out-of-range ratios.

**Nested Azure study hosts (all 9 VMs):** `/sys/module/kvm_intel/parameters/tsc_scaling` was not readable from inside any study VM (fingerprints record `kvm_intel_tsc_scaling` as empty on every host across D4s_v3, D4s_v4, and D4s_v5). We could not enable or inspect the module parameter from the guest. When a restore pair requires TSC scaling (snapshot `tsc_khz` differs from the restore host by >250 ppm), `KVM_SET_TSC_KHZ` returned `Invalid argument (os error 22)` — surfaced by Firecracker as `Could not set TSC scaling within the snapshot`. With `none`, that occurred on 3 restore attempts across 3 creator→restorer pairs (8272CL or Broadwell creators → 8370C D4s_v3 or 8272CL restorers); other pairs either did not need scaling or passed without hitting this path. The missing sysfs knob is uniform on every study VM; the scaling **error** is pair-direction-specific, not universal on every restore.

**Bottom line:** Firecracker detects the mismatch and invokes KVM; **KVM accepts or rejects** the scale. Neither layer can “template away” TSC for snapshots — they either scale successfully or restore fails.

## Ice Lake ↔ Cascade Lake (ignoring Broadwell)

Between 8370C D4s_v3 and 8272CL D4s_v4 only:

| Creator → restorer | none / T2 / C3 | Failure mode |
| --- | --- | --- |
| 8370C D4s_v3 → 8272CL D4s_v4 | pass | — |
| 8272CL D4s_v4 → 8370C D4s_v3 | fail | TSC scaling |

So for this pair, TSC was the sole blocker — templates did not change the outcome.

That does not mean “without TSC, everything works fleet-wide.” The same 8370C marketing name on D4s_v5 (different fingerprint) still failed restores to D4s_v3 and 8272CL with partial MSR restore, not TSC. Azure SKU/stepping drift adds MSR/CPUID state beyond generation labels.

## What T2/C3 are good for

They solve a **different problem** from snapshot portability: make a heterogeneous fleet look homogeneous for **cold boots** — consistent CPUID, instruction sets, and (for T2S) security-capability MSRs so apps see the same CPU features on every host.

Use cases from [Firecracker’s template docs](https://github.com/firecracker-microvm/firecracker/blob/main/docs/cpu_templates/cpu-templates.md):

- Run the same container/workload on D4s_v3 and D4s_v5 without CPUID surprises.
- Mirror AWS T2/C3 instance CPU profiles.
- **T2S:** snapshot on newer Intel, restore on older Skylake/Cascade ([PR #3066](https://github.com/firecracker-microvm/firecracker/pull/3066)) — mainly `ARCH_CAPABILITIES` / security parity, **not** TSC rewriting.

They do **not** promise arbitrary snapshot restore across hosts. [Snapshot CPU guidance](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/versioning.md#cpu-model) is separate: saved vCPU state must be restorable on the target.

## Why T2S was not needed for newer → older (despite PR #3066)

[PR #3066](https://github.com/firecracker-microvm/firecracker/pull/3066) adds the T2S template to force `MSR_IA32_ARCH_CAPABILITIES` (and related CPUID bits) to Skylake-class security capabilities so a snapshot taken on a **newer** host can restore on an **older** Skylake/Cascade host without guest software seeing newer mitigations that the older CPU lacks. It explicitly does **not** address TSC frequency or `MSR_IA32_TSC`.

In our study, the canonical newer→older Platinum pair already worked **without** T2S:

| Creator → restorer | `none` / T2 / C3 | With T2S |
| --- | --- | --- |
| 8370C D4s_v3 (Ice) → 8272CL D4s_v4 (Cascade) | pass | **fail** (invalid `snapshot_state` on same-host restore for some creators) |

T2S was aimed at Skylake↔Cascade-era AWS fleets; our Azure nested-KVM hosts are Ice/Cascade with different failure modes (TSC scaling, MSR partial write, XCR on Broadwell). For the Ice→Cascade direction that PR #3066 describes, `none` already passed — T2S added no benefit and regressed (8370C v3 → 8272CL with T2S = fail).

The **TSC-limited** direction (8272CL → 8370C D4s_v3) is a separate problem: restore host TSC is “faster” and KVM rejects scaling the snapshot’s embedded guest TSC rate — T2S cannot fix that.

## Partial MSR restore on 8370C D4s_v5

Azure assigned the same marketing name (8370C) on D4s_v3 and D4s_v5, but our fingerprinting found **two distinct signatures** (`7defc2ef…` on D4s_v3 vs `be703f15…` on D4s_v5). Restore is **asymmetric**:

| Direction | Result | Error |
| --- | --- | --- |
| 8370C D4s_v3 → 8370C D4s_v5 | pass | — |
| 8370C D4s_v5 → 8370C D4s_v3 | fail | partial MSR restore |
| 8370C D4s_v5 → 8272CL D4s_v4 | fail | partial MSR restore |
| 8370C D4s_v5 → E5-2673 D4s_v3 | fail | XCR (not MSR) |

Error text: `Failed to set all KVM MSRs for this vCPU. Only a partial write was done.`

**What this means:** Firecracker calls `KVM_SET_MSRS` with the full MSR list saved in the snapshot. KVM writes MSRs sequentially and stops at the first rejection; Firecracker treats any incomplete write as fatal ([`VcpuSetMsrsIncomplete`](https://github.com/firecracker-microvm/firecracker/blob/main/src/vmm/src/arch/x86_64/vcpu.rs) — Firecracker compares `nmsrs` written vs requested). The snapshot captured MSR values from the D4s_v5 host’s KVM view; the D4s_v3 (or 8272CL) host’s KVM refuses one or more indices — commonly `MSR_IA32_ARCH_CAPABILITIES`, `MSR_IA32_SPEC_CTRL`, platform/power MSRs, or other model-specific registers that differ between fingerprints even under the same CPU marketing name.

This is distinct from TSC scaling (8272CL → 8370C v3) and from XCR replay onto Broadwell. It is why “same generation” or “same SKU” is insufficient: Azure stepping/microcode/platform differences surface as non-restorable MSR blobs in saved vCPU state. Templates at create time did not fix v5→v3/v4 partial MSR failures in our matrix.

## Why TSC scaling is directional (8272CL → 8370C fails; reverse passes)

Our study matches Firecracker’s documented newer → older snapshot migration story (T2S targets Skylake/Cascade), not symmetric migration:

- 8370C (Ice) → 8272CL (Cascade): pass — snapshot taken on newer/faster host; older host accepts guest TSC state (KVM scaling succeeds or frequencies are close enough).
- 8272CL → 8370C D4s_v3: fail — snapshot embeds Cascade-era TSC timing; restoring on Ice Lake, `KVM_SET_TSC_KHZ` / scaling fails on nested Azure KVM.

Rough model: the snapshot records “run at this TSC rate with this counter state.” Moving down to an older/slower host often works; moving up to a newer host may require scaling the guest TSC in ways nested KVM on Azure rejects. We could not tune `kvm_intel.tsc_scaling` from the guest on study VMs.

This is platform behavior, not a missing template at restore — production never applies `/cpu-config` on load.

## How others handle cross-host snapshot restore

Firecracker upstream treats cross-model restore as fragile; there is no universal “fix TSC everywhere” patch ([issue #2985](https://github.com/firecracker-microvm/firecracker/issues/2985) is parked). Patterns in the wild:

| Approach | What it does | Limits |
| --- | --- | --- |
| Same CPU fingerprint | Create and restore on identical host CPU | Default Firecracker recommendation |
| Homogeneous pools / routing | Scheduler pins snapshot to compatible hosts | Requires fingerprinting (our production suggestion) |
| One-way migration | Create snapshots on newer CPU; restore only on same or older generation | Matches our passing Ice → Cascade direction |
| T2S at golden snapshot create | Restrict guest features to Skylake-class security/CPUID for newer→older | Performance cost; our T2S results were worse, not better |
| Bare metal, same instance type | AWS documents limited snapshot reuse on identical metal SKUs + kernel pairs | Not VMSS; still not cross-SKU ([snapshot-support.md](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/snapshot-support.md)) |
| Cold boot fallback | Skip snapshot restore when incompatible | Slower but reliable |
| Kernel/hypervisor tuning | `kvm_intel.tsc_scaling=1`, host TSC invariant | Not exposed on our Azure nested-KVM study hosts |
| Live migration (QEMU/KVM) | Explicit `tsc-freq`, migration-time TSC rate save/restore | Different stack; still fails or falls back when scaling unsupported |

Nobody “templates away” TSC for snapshots — they avoid the mismatch (pinning, direction, or cold start).

## Custom templates — worth another try?

Low priority for our Azure VMSS goal (snapshot on any host, restore elsewhere):

- Naive CPUID intersection failed at create on every host (KVM rejected leaf `0xb:1`).
- Firecracker’s intended workflow is [`template dump` → `template strip` → manual edit → `template verify`](https://github.com/firecracker-microvm/firecracker/blob/main/docs/cpu_templates/cpu-template-helper.md) — not bitmap-AND across dumps.
- Even a verified custom template would still not cover TSC (excluded MSR) or XCR replay onto drift hosts.

Reasonable to retry only if: fleet is narrowed to Ice+Cascade only, configs are built with strip+verify per host, and success is measured as “same as none on CPUID-limited pairs” — not “solve TSC.” Expected ROI is low compared to fingerprint routing + cold-boot fallback.

## Excluded MSRs in `cpu-template-helper` dumps

When building custom templates, [`cpu-template-helper template dump`](https://github.com/firecracker-microvm/firecracker/blob/main/docs/cpu_templates/cpu-template-helper.md) outputs guest CPU state in `/cpu-config` JSON. The [appendix “MSRs excluded from guest CPU configuration dump”](https://github.com/firecracker-microvm/firecracker/blob/main/docs/cpu_templates/cpu-template-helper.md) lists registers that are omitted from dump/strip/compare because they are not reasonable to modify via templates — e.g. `MSR_IA32_TSC`, performance counters, VMX MSRs, hypervisor MSRs.

Practical meaning:

- Custom templates (and our intersected helper configs) cannot normalize TSC across hosts — TSC is not in the templateable set.
- Dump/intersect workflows only cover the modifiable subset; expecting intersection to fix snapshot portability across generations ignores TSC and other excluded state that snapshots still carry.
- Verify/strip commands operate on the same modifiable subset; they do not validate snapshot-restore compatibility.

## Why portability is limited

Snapshots are not like disk images. Restore asks the hypervisor to recreate low-level CPU state. If the target host is newer, older, or exposes different features (common on shared cloud hardware), restore fails even when the guest OS and workload are fine.

| Symptom | Underlying cause | Typical pattern in this study |
| --- | --- | --- |
| TSC / timer scaling | Guest TSC frequency in snapshot cannot be adapted | 8272CL → 8370C D4s_v3 |
| XCR registers | Snapshot XCR state cannot be applied on target | Any newer Intel → E5-2673 v4 |
| KVM MSRs | Partial MSR restore | 8370C D4s_v5 → 8272CL |
| Invalid snapshot at create | Corrupt `snapshot_state` on same host | T2/C3/T2CL/T2S on Broadwell; T2S on some 8370C |

## Template verdicts

| Approach | Verdict | Notes |
| --- | --- | --- |
| No template (`none`) | Use | 4/12 cross-host pass; same-host reliable |
| Static templates (T2, C3, T2CL) | Avoid on mixed pools | Same TSC/XCR failures; invalid snapshots on Broadwell at create |
| T2S | Avoid | 3 same-host failures |
| no-xcrs | No benefit | Identical pass/fail matrix to `none`; XCR errors unchanged |
| Custom helper configs | Avoid | 32 cells `create_failed` with intersected configs |

Production restore uses `/snapshot/load` only — templates must be baked in at golden snapshot create, not applied at restore.

## Host environment

Nested KVM on Azure study VMs (9 runners, three cohorts): `kvm_intel.nested=Y` and `kvm_intel.ept=Y` on all Intel hosts. `kvm_intel.tsc_scaling`: not readable from the guest on **9/9** VMs (empty in host fingerprints — `collect-host-fingerprint.sh` reads `/sys/module/kvm_intel/parameters/tsc_scaling` only when the path exists and is readable). We did not probe `KVM_CAP_TSC_CONTROL` directly; the observed `Could not set TSC scaling within the snapshot` errors on cross-frequency restores are consistent with limited TSC multiplier support in this nested stack.

Cross-generation restore remains best-effort on this infrastructure.

Per-cell logs: `results/fc-snap-hetero-1781257524/summary.md`, `results.jsonl`.
