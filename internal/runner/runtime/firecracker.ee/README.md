# Firecracker Runner Runtime

This runtime starts each sandbox as a Firecracker microVM restored from a
prebuilt snapshot. It is intended for VM/VMSS hosts where the runner owns the
host Firecracker setup and local snapshot cache.

Entry point: `cmd/runner-firecracker.ee/`.

## Technology

- Uses upstream Firecracker and jailer.
- Restores a full memory snapshot and VM state through the Firecracker API.
- Runs each microVM in a Linux network namespace with a TAP device.
- Exposes the guest daemon through a host-local TCP proxy.

## Networking

The Firecracker backend follows the Lambda/Firecracker PoC shape: each sandbox
gets a per-slot Linux network namespace with a TAP device, and the runner
exposes the guest daemon through a host-local TCP proxy. `DaemonURL` is therefore
a `127.0.0.1:<port>` URL, while proxy connections are dialed from inside the
sandbox netns to the guest IP.

We need slots because Firecracker does not provide Docker-style bridge networking
or container names for free. Each microVM clone needs its own host network
namespace and a TAP device with the snapshot's expected name inside that
namespace. The runner also needs a deterministic host-local `DaemonURL` for its
existing HTTP proxy. Slots are the small accounting layer that ties those
resources together and prevents two sandboxes from trying to use the same netns,
TAP, or proxy port.

Slots are deliberately runner-local and ephemeral. They are not persisted, not
part of the public API, and not a promise that the same sandbox ID will always
get the same slot after restart.

## Supported Features

- Tracks basic runner-local slot capacity.
- Validates required Firecracker binaries and snapshot assets in readiness.
- Starts Firecracker through jailer and restores the configured snapshot.
- Creates per-sandbox network namespace/TAP state.
- Exposes the guest daemon through a host-local proxy URL.
- Waits for guest daemon `/healthz` before returning a sandbox as ready.
- Cleans up the VM process, proxy, jail state, and network namespace on
  stop/delete or create failure.

## Current Limitations

- Slots are allocated in memory and are not pre-created or persisted.
- Stop and delete both tear down the VM; stopped VM reuse is not implemented.
- The snapshot/rootfs set must be built together and include the n8n sandbox
  daemon listening on the configured daemon port.

## Host and asset notes

Firecracker hosts must expose KVM to the runner (nested-virtualization-capable Azure
D/E-series, `/dev/kvm`, Intel `nested=Y` and `ept=Y`). The e2e path builds kernel,
rootfs, and golden snapshot on the target VM from pinned Firecracker CI inputs and
the locally built sandbox daemon.

## Snapshot restore across hosts

Restore is CPU-sensitive on heterogeneous VMSS pools. Production uses `/snapshot/load`
only — no `/cpu-config` at restore. See the
[snapshot portability report](../../../../docs/firecracker-intel-snapshot-compat-report.md)
for failure modes, template guidance, and operational mitigations.

Study tooling: [snapshot compatibility study](../../../../scripts/firecracker-snapshot-compat/README.md).
