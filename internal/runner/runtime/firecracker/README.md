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

## Security Hardening TODO

- Keep one proxy per sandbox and close it before releasing the slot.
- Add tests for slot reuse and cleanup.
- Add file descriptor/timeouts/connection limits.
- Consider Unix sockets instead of TCP localhost if the Go reverse proxy path allows it later.
- Add guest-daemon authentication or per-sandbox secret headers if we want defense in depth.
- Eventually pre-create network slots and verify cleanup before reuse.
