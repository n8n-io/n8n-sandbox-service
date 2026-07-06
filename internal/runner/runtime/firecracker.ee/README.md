# Firecracker Runner Runtime

This runtime starts each sandbox as a Firecracker microVM restored from a
prebuilt snapshot. It is intended for VM/VMSS hosts where the runner owns the
host Firecracker setup and local snapshot cache.

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
- Clones the golden template rootfs and snapshot assets to a per-sandbox data
  directory at `SANDBOX_RUNNER_DATA_DIR/<sandbox_id>/` before jail setup.
- Stops running sandboxes via pause + snapshot/create, persisting per-sandbox
  `snapshot_mem` and `snapshot_state` files for later wake.
- Wakes stopped sandboxes by restoring the per-sandbox snapshot on demand
  (`EnsureSandboxRunning`), with singleflight deduplication for concurrent wakes.
- Creates per-sandbox network namespace/TAP state.
- Exposes the guest daemon through a host-local proxy URL.
- Waits for guest daemon `/healthz` before returning a sandbox as ready.
- Cleans up the VM process, proxy, jail state, per-sandbox data directory, and
  network namespace on delete or create failure. Stopped sandboxes keep their
  data directory until delete or LRU eviction when disk space is low.

## Resource limits

CPU, memory, and disk are **not** configured through runner environment
variables (unlike the Docker/sysbox backend). They are fixed when the golden
snapshot and template `rootfs.ext4` are built:

- **CPU and memory** — baked into the snapshot (`vcpu_count`, `mem_size_mib` in
  the golden-snapshot build; see `e2e/infra/scripts/create-golden-snapshot.sh`).
- **Disk** — capped by the ext4 image size of the golden template
  (`rootfs.ext4`; see `FIRECRACKER_E2E_ROOTFS_SIZE_MB` in
  `e2e/infra/scripts/setup-firecracker-e2e-vm.sh` for the e2e default). Each
  sandbox gets a sparse copy of that image at create time.

To change limits in production, rebuild the golden snapshot/rootfs on the host
rather than tuning runner env vars.

## Current Limitations

- Slots are allocated in memory and are not pre-created or persisted.
- LRU eviction of stopped sandboxes for disk space is runner-local and does not
  notify the API.
- The snapshot/rootfs set must be built together and include the n8n sandbox
  daemon listening on the configured daemon port.
