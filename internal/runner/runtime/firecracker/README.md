# Firecracker Runner Runtime

This runtime starts each sandbox as a Firecracker microVM restored from a
prebuilt snapshot. It is intended for VM/VMSS hosts where the runner owns the
host Firecracker setup and local snapshot cache.

## Technology

- Uses upstream Firecracker and jailer.
- Restores a full memory snapshot and VM state through the Firecracker API.
- Runs each microVM in a Linux network namespace with a TAP device.
- Exposes the guest daemon through a host-local TCP proxy.

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

## Host and Asset Notes

Firecracker hosts must expose KVM to the runner. The e2e VM setup now fails
fast unless `/dev/kvm` exists, CPU virtualization flags are present, and the
relevant KVM module parameters report nested virtualization support. On Intel
hosts this includes `nested=Y` and `ept=Y`.

The Firecracker e2e path creates the kernel/rootfs template and snapshot on the
same VM that runs the tests. It installs a pinned upstream Firecracker release,
downloads the matching upstream Firecracker CI kernel/rootfs inputs, injects the
locally built sandbox daemon, and then creates `snapshot_mem` and
`snapshot_state` locally.

The generated rootfs owns the guest filesystem contract: it includes the
sandbox user, `/home/user`, and a writable sticky `/tmp`. The guest daemon may
start as kernel `init`, but it drops to UID/GID 1000 before serving API requests,
so daemon file operations and workload commands run as the sandbox user.

The e2e asset contract is the configured Firecracker release, the matching
Firecracker CI kernel/rootfs inputs, and the locally built sandbox daemon. The
target VM generates the full bootable template and snapshot from those pinned
inputs locally.

Snapshot restore is CPU-sensitive. Firecracker documents that snapshots are only
compatible when the guest-visible CPU features are invariant between snapshot
creation and restore. For production VMSS, either create snapshots on each host
or enforce a homogeneous guest CPU contract, for example by constraining host
placement and/or using Firecracker CPU templates.

## Security Hardening TODO

- Keep one proxy per sandbox and close it before releasing the slot.
- Add more tests for slot reuse and cleanup.
- Add file descriptor/timeouts/connection limits.
- Consider Unix sockets instead of TCP localhost if the Go reverse proxy path allows it later.
- Add guest-daemon authentication or per-sandbox secret headers if we want defense in depth.
- Eventually pre-create network slots and verify cleanup before reuse.
