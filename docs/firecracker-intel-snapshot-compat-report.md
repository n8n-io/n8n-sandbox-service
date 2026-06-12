# Firecracker snapshot portability on heterogeneous hosts

Empirical study: 4 distinct Intel CPU signatures on Azure VM-scale sets with nested KVM.

Raw data: [`results/fc-snap-hetero-1781257524/`](../scripts/firecracker-snapshot-compat/results/fc-snap-hetero-1781257524/)

## Summary

Firecracker snapshots freeze CPU state (timers, registers, CPUID) as well as memory. On a cloud scale set, hosts are not guaranteed to be CPU-identical — even under the same VM SKU — and Azure will keep rolling out newer hardware over time. You cannot assume that a snapshot taken on one runner will restore on another.

Restore on the same CPU fingerprint as the creator, or on a small set of known-compatible hosts (often same generation, sometimes only in one direction) works. Treating the fleet as one interchangeable snapshot pool without checks does not.

Recommendation: do not use CPU templates for a heterogeneous Azure pool. Static templates (T2, C3, T2CL, T2S) did not make cross-host restore reliable and could produce invalid snapshots on some hosts. Custom intersected configs failed at create time. Stay on the host-native CPU model at snapshot time unless you run a homogeneous, validated fleet.

Running in production (snapshot on any host, restore on another):

1. Fingerprint + route — Record the host CPU fingerprint when creating a snapshot; only restore on hosts in a compatible allow-list (same fingerprint first; expand only where testing proves it).
2. Pools by generation — Split runners into snapshot pools per CPU generation; never move snapshots across pools. Re-baseline when Azure introduces new CPUs.
3. Graceful fallback — If restore fails compatibility checks or `/snapshot/load` errors, cold-start from the golden disk image instead of the snapshot (accept slower recovery rather than a broken VM).

## Why portability is limited

Snapshots are not like disk images. Restore asks the hypervisor to recreate low-level CPU state. If the target host is newer, older, or exposes different features (common on shared cloud hardware), restore fails even when the guest OS and workload are fine.

Two operational facts matter for any VMSS design:

- You may not be able to standardize on one CPU — the same SKU can map to different steppings; outliers appear over the fleet lifetime.
- The fleet will drift forward — new instance types and CPU generations join over time; snapshots from today are not automatically valid on tomorrow’s hosts.

## What breaks on restore

| Symptom | Underlying cause | Typical pattern |
|---------|------------------|-----------------|
| TSC / timer scaling | Guest time-stamp counter frequency in the snapshot cannot be adapted to the target host | Often older → newer generation fails; reverse may work |
| XCR / extended registers | Register state from the snapshot cannot be applied on the target | Newer → much older hosts (e.g. legacy cores in the same SKU family) |
| MSR mismatch | Model-specific registers differ between creator and restorer | Mixed generations or outliers |
| Invalid snapshot at create | Snapshot file already corrupt on the same host | Some CPU templates on drift hosts — restore never had a chance |

Same-host restore (or restore on a fingerprint-matched host) remained reliable in the study. Cross-host success was partial and directional, not universal.

## CPU templates and “cpu profiles”

### Template verdicts (study)

| Approach | Verdict | Notes |
|----------|---------|-------|
| No template (`none`) | Use | Baseline; same-host reliable; no extra create-time risk |
| Static templates (T2, C3, T2CL) | Avoid on mixed pools | No meaningful portability gain; can create invalid snapshots on some hosts |
| T2S | Avoid | Worst create-time failure rate in the study (3 same-host failures) |
| no-xcrs | No benefit (Intel-only) | Same cross-host outcomes as no template; does not fix TSC |
| Custom helper configs | Avoid | Intersected configs failed snapshot create on every host (32 cells) |

Templates can mask some CPUID differences in theory; they do not fix TSC across generations or outlier hardware in a scale set. Production restore uses `/snapshot/load` only — templates must be baked in at golden snapshot create, not applied at restore.

## Host environment (nested KVM on Azure)

On nested-KVM study VMs: virtualization features were available, but TSC scaling could not be tuned from the guest (`kvm_intel.tsc_scaling` not exposed). That reinforces treating cross-generation restore as best-effort, not guaranteed.

## Study specifics (reference)

The matrix used 4 Intel signatures across D4s_v3 / D4s_v4 / D4s_v5 — including cases where the same SKU exposed different CPUs (e.g. modern Platinum vs legacy Broadwell on D4s_v3). Per-cell results: `summary.md`, `results.jsonl` under `results/fc-snap-hetero-1781257524/`.
