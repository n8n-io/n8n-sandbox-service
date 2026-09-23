# Firecracker runner runtime

Each sandbox is a Firecracker microVM restored from a golden snapshot built on the host. Upstream Firecracker and jailer; one runner process per host (netns/veth names `fc-sb-{n}`, `fc-veth-{n}` would collide otherwise). Host setup is in [quickstart-firecracker-linux.md](../../../../docs/quickstart-firecracker-linux.md); env vars in [configuration.md](../../../../docs/configuration.md#firecracker-runner-backend-config).

## Admission (`Prepare` / `Ready`)

The runner reports `Healthy=false`, fails `/readyz` and `Ready()` until all of these pass, retrying with backoff:

1. Host NAT — IPv4 forwarding plus idempotent MASQUERADE/FORWARD rules.
2. Guest assets — jailer, firecracker, `template/rootfs.ext4`, `template/vmlinux`, and optionally `MANIFEST.json` / expected `git_sha` / daemon checksum.
3. Golden snapshot — if `snapshot_mem`, `snapshot_state` or `boot.json` is missing and `SANDBOX_RUNNER_FIRECRACKER_CREATE_SNAPSHOT_SCRIPT` is set, run `create-golden-snapshot.sh` locally (snapshots are not portable across CPU generations). The runner passes its own `GUEST_IP`, `HOST_TAP_IP_CIDR`, `HOST_TAP_DEVICE_NAME` and `DAEMON_PORT`; `MEM_MIB` and `VCPUS` are script-owned.
4. Snapshot pin — mem/state exist and `boot.json` agrees with the runner (see [boot.json](../../../../docs/configuration.md#golden-snapshot-boot-parameters-bootjson)).
5. Canary — create `admission-canary-*`, probe `/healthz`, run a tiny exec and a files round-trip, delete it. A canary whose cleanup fails keeps its slot and fails admission, so a capacity-1 runner is never healthy with a leaked slot.

## Networking

Each sandbox slot gets its own network namespace:

| Piece | Role |
|-------|------|
| TAP `fc-tap-0` | Virtio NIC; gateway `172.16.0.1`, guest `172.16.0.10` (baked into the snapshot) |
| veth `fc-uplink` (netns) ↔ `fc-veth-{slot}` (host) | Routes guest traffic to the host routing table |
| Host proxy `127.0.0.1:{port}` | Runner listens on the host, dials the guest daemon from inside the netns |

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

`network/network.go` owns topology (netns, TAP, veth, routes, NAT); `network/egress.go` owns the private-CIDR `FORWARD` rules (Docker `netpolicy` parity) and the egress `none` rule, a `FORWARD -i <tap> -j DROP` inserted ahead of them. Guest IPv6 is disabled via `ipv6.disable=1` in the snapshot boot args; changing boot args requires a snapshot rebuild.

## Slots and lifecycle

A slot ties one netns, TAP, veth and proxy port together. Slots are in-memory, runner-local and not stable across restarts: a stopped sandbox releases its slot and may wake onto another.

Nothing in the namespace build depends on the sandbox that will use it, so a background wirer (`network_wirer.go`, started from `Prepare`) builds every free slot at startup and rebuilds each slot after release. Activation skips the build on a wired slot (`setup_network_ms` ≈ 0), waits on one mid-build, and builds inline on one the wirer has not reached. Readiness does not wait for wiring, and teardown still deletes the namespace, so every sandbox gets a fresh one. `Shutdown` clears the namespaces of free slots, best-effort; startup reconcile sweeps whatever that leaves. `sandbox_slots_unwired` reports how many slots are not built; zero in steady state. The egress `none` DROP is the one sandbox-specific rule, so activation adds it on top of the built namespace, on create and on every wake; pre-wired namespaces only ever carry the default policy.

Invariants (rationale in the code comments of `stop_wake.go`, `crash.go`, `network_wirer.go`, `runtime.go`):

- Create, stop, wake and delete are mutually exclusive per sandbox (`beginTransition`). Each claim runs under a fixed budget detached from the caller's cancellation, so a disconnected client or wedged host command cannot leave a sandbox half torn down. `Shutdown` is the one operation that does not wait.
- A stop releases its slot even if host cleanup fails; the sandbox becomes an ordinary stopped one. A failed delete keeps its slot because the API retries deletes every sweep; a failed create releases it because nothing will ever retry.
- Teardown marks its slot unwired, and slot setup deletes leftover netns/veth before creating them, so a slot handed back with leftovers comes up clean.
- A teardown never touches a slot another sandbox holds: `clearSlotNetwork` runs under the slot's network lock and re-reads the owner there, per incarnation, because `Shutdown` can release a slot from under a concurrent teardown while the next sandbox takes it. A stopped sandbox has no slot by the time it is deleted, so only its jail is cleaned.
- Every microVM incarnation has a `generation`; an exit callback carrying an old generation is ignored, which is how runner-initiated kills are told apart from guest deaths.

## Guest death and cold boot

The guest daemon is PID 1 with `panic=1 reboot=k`, and jailer execs Firecracker in place, so a guest dying ends the process the runner is waiting on. The runner tears the sandbox down and releases its slot, leaving it stopped with files intact. It is never restored from its now-stale snapshot (a memory image whose cached filesystem metadata predates later disk writes corrupts the rootfs silently); instead the next request cold boots the existing rootfs with the boot parameters recorded in `boot.json`, and is answered with `409 sandbox_restarted` ([API.md](../../../../docs/API.md#http-409-sandbox_restarted--the-sandbox-came-back-without-its-memory)). `boot.go` replays the same five Firecracker API calls `create-golden-snapshot.sh` uses. Behavior and metrics: [architecture.md](../../../../docs/architecture.md#recovering-a-crashed-guest), [observability.md](../../../../docs/observability.md#runner-guest-deaths).

## Resource limits

CPU, memory and disk are fixed when the golden snapshot and `rootfs.ext4` are built, not by runner env vars: `VCPUS`/`MEM_MIB` in `create-golden-snapshot.sh`, `FIRECRACKER_ROOTFS_SIZE_MB` (default 2048) in `build-rootfs-template.sh`. Each sandbox gets a sparse copy of the rootfs; filling it yields ENOSPC in the guest. Rebuild on the host to change limits.

## Limitations

- Sandboxes are not reattached after a runner restart; startup reconcile removes orphaned data directories, jail state and slot namespaces (a jail directory still holding an active bind mount is logged and left in place).
- LRU eviction of stopped sandboxes when disk is low is runner-local and does not notify the API.
- Egress policy is per-netns iptables, not nftables sets; no per-sandbox allowlists.
