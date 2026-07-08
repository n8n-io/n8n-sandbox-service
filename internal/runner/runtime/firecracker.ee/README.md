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

Each sandbox slot gets an isolated Linux network namespace. Three interfaces
matter:

| Piece | Role |
|-------|------|
| **TAP** (`fc-tap-0`) | Virtio NIC the microVM talks to; gateway `172.16.0.1`, guest `172.16.0.10` (baked into the golden snapshot). |
| **veth uplink** (`fc-uplink` in netns ↔ `fc-veth-{slot}` on host) | Routes guest traffic out of the netns to the host routing table. |
| **Host proxy** (`127.0.0.1:{port}`) | API/exec path: runner listens on the host, dials the guest daemon from inside the netns. |

```
Guest 172.16.0.10 ── virtio ── TAP (172.16.0.1)
                                  │
                       netns fc-sb-{slot}   [FORWARD: drop private CIDRs]
                                  │
                         veth fc-uplink
                                  │
Host fc-veth-{slot} ── FORWARD ── MASQUERADE ── internet

API/exec: 127.0.0.1 proxy ── setns ── guest:8081
```

**`network/`** owns sandbox networking: topology (netns, TAP, veth, routes, NAT)
in `network.go`, egress policy (private-CIDR `iptables FORWARD` rules) in
`egress.go`. Host `ip_forward` and default interface MASQUERADE are verified
idempotently at runner startup.

Guest IPv6 is disabled via `ipv6.disable=1` in the golden snapshot kernel boot
args — rebuild the snapshot after changing boot args.

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

Run **one Firecracker runner process per host**. Multiple runners on the same
machine collide on Linux netns/veth names (`fc-sb-{n}`, `fc-veth-{n}`); use
separate VMs (or containers with isolated network namespaces) for multi-runner e2e
and production layouts.

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
- Creates per-sandbox network namespace/TAP state with veth uplink and private-CIDR
  egress filtering (Docker `netpolicy` parity).
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
  sandbox gets a sparse copy of that image at create time. There is no
  `SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB` equivalent; ENOSPC occurs when the
  guest fills that ext4 device.

To change limits in production, rebuild the golden snapshot/rootfs on the host
rather than tuning runner env vars.

## Current Limitations

- Slots are allocated in memory and are not pre-created or persisted.
- On runner startup, orphaned per-sandbox data directories, jailer state, and
  slot network namespaces are removed. Sandboxes are not reattached after a
  runner restart (same contract as the Docker runner reconcile).
- LRU eviction of stopped sandboxes for disk space is runner-local and does not
  notify the API.
- Per-sandbox egress uses per-netns iptables (not nftables sets or a pre-wired
  slot pool); optimize if create latency becomes critical.
- The snapshot/rootfs set must be built together and include the n8n sandbox
  daemon listening on the configured daemon port.
