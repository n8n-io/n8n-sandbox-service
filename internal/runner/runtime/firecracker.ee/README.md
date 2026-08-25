# Firecracker Runner Runtime

This runtime starts each sandbox as a Firecracker microVM restored from a
prebuilt snapshot. It is intended for VM/VMSS hosts where the runner owns the
host Firecracker setup and local snapshot cache.

## Technology

- Uses upstream Firecracker and jailer.
- Restores a full memory snapshot and VM state through the Firecracker API.
- Runs each microVM in a Linux network namespace with a TAP device.
- Exposes the guest daemon through a host-local TCP proxy.

## Admission (Prepare / Ready)

On startup `Prepare` gates the runner before heartbeats report healthy:

1. Host NAT — enable IPv4 forwarding and idempotent MASQUERADE/FORWARD rules. Transient failures are retried with admission backoff (success is cached; failures are not).
2. Pin guest assets — jailer, firecracker, `template/rootfs.ext4`, `template/vmlinux`, and optional `MANIFEST.json` / expected `git_sha` / daemon checksum.
3. Ensure golden snapshot — if `snapshot_mem`, `snapshot_state` or `boot.json` is missing and `SANDBOX_RUNNER_FIRECRACKER_CREATE_SNAPSHOT_SCRIPT` is set, run `create-golden-snapshot.sh` on this host (snapshots are not portable across CPU gens). Production Firecracker VM images set this so the runner owns snapshot creation after first-boot builds the rootfs template. The runner passes its own `GUEST_IP`, `HOST_TAP_IP_CIDR`, `HOST_TAP_DEVICE_NAME` and `DAEMON_PORT` to the script so the snapshot it builds is one step 4 accepts; `MEM_MIB` and `VCPUS` stay script-owned because the runner has no configuration for them.
4. Pin snapshot assets — mem/state exist, and `boot.json` parses and agrees with the runner on `guest_ip`, `host_tap_device_name` and `daemon_port` (see [Boot parameters](#boot-parameters-bootjson)).
5. Admission canary — create a throwaway sandbox (`admission-canary-*`), probe `/healthz`, a tiny `POST /executions`, and a files put/get round-trip, then delete it. Canary deletion must fully release its slot; cleanup failure fails admission (and retries purge leftover canaries) so capacity-1 runners are never marked healthy with a permanently consumed slot.

Until admission succeeds, `Ready()` fails, `/readyz` is not ready, and registration heartbeats send `Healthy=false`. Failed admission retries with backoff while the process runs.

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
get the same slot after restart. A sandbox that stops releases its slot and may
wake onto a different one.

A stop releases its slot even when the host cleanup that follows fails. The
failure is logged and returned, but the state moves on to stopped, because that is
the only outcome that leaves the sandbox usable. `teardownRunningVM` drops the
process and proxy handles whatever it returns, so no retry can resume where it
stopped — and a sandbox left marked running would hold its slot for a microVM
nothing can reach, hand out a daemon URL nothing listens on, and fail every later
stop against an API socket that died with its guest. Nothing would ever reclaim it.
The snapshot is written by then, so the sandbox becomes an ordinary stopped one and
wakes on the next request instead; a slot handed back with leftovers on it comes up
clean anyway, because the next sandbox clears the slot before building it.

That last part is load-bearing for every path that releases a slot after a failed
teardown, so it is worth being precise about which resources are per-slot. The netns
`fc-sb-{slot}` and the host veth `fc-veth-{slot}` are, and the setup script deletes
both before creating them — including the veth, without which a single leftover
would fail `ip link add` under `set -eu` and strand the slot until the runner
restarts. Deleting a netns by name works even while a stale microVM still runs
inside it: the kernel keeps that namespace alive for the process while freeing the
name, so the new sandbox gets an empty namespace and the stale guest is isolated in
an unnamed one with its uplink gone. The proxy port is per-slot too, and is freed by
the `Stop` that teardown performs on every handle it claims; were it ever left
bound, the next create on the slot would fail loudly on bind rather than share it.
The jail directory is keyed to the `vmID`, so it collides with nothing and startup
reconcile sweeps it.

A failed **delete** keeps its slot, which looks inconsistent with that but is not.
Cleanup runs as a single shell command, so a failure that is not the jail directory
it removes last — an expired context, a `sudo` that never ran — leaves it unknown
whether the netns is really gone. Delete can afford that caution where stop cannot,
because a delete retry does arrive: the API keeps the sandbox row when the runner
reports a delete failure, and the idle sweeper retries every sweep interval. The
retry repeats host cleanup, removes the data directory, and releases the slot.

Host cleanup on delete runs whether or not the sandbox still has process and proxy
handles, which matters because a stop or crash whose cleanup failed marks the sandbox
stopped and drops those handles anyway. Skipping cleanup on the strength of them
would leave the jail's bind mounts in place while the delete removes the data
directory beneath them, so the snapshot files would stay allocated with no way left
to reclaim them: startup reconcile cannot help either, since its `rm -rf` fails on a
directory holding an active mount. Repeating cleanup is safe because every step is
guarded and what it removes is keyed to the `vmID`.

The admission canary is built on this — a canary whose
cleanup fails keeps its slot and fails admission, so a capacity-1 runner is never
marked healthy while one of its slots is unaccounted for.

The cleanup a **failed create** runs is the exception to that: it releases the slot,
because it is the one delete with nothing behind it. The create returns an error, so
the API stores no record for the sandbox, and without a record neither an explicit
delete nor the idle sweeper can ever name it again. Keeping the slot would strand
runner capacity until the process restarts, so this path follows stop rather than
delete and hands it back.

Because slot ownership moves like that, create, stop, wake, and delete are
mutually exclusive per sandbox: each claims the sandbox for the duration of the
transition and the others wait. Without that, a delete overlapping a create or
wake would run before the microVM, proxy, and slot-derived fields were published,
skip teardown, and leave an orphaned microVM holding host resources on a slot the
runner had already handed back. The delete claim is terminal and doubles as the
tombstone that hides the sandbox from lookups, so a single flag decides both
mutual exclusion and visibility. `Shutdown` is the one exception to waiting — it
sweeps sandboxes mid-stop, mid-wake or mid-guest-death, since a claim it waited for
could outlive the process and leave a microVM running past the runner (jailer's
children are their own process group, so they do not die with it). It still skips
sandboxes a delete already claimed, so a delete never runs twice.

Because of that exception, the claim cannot be the only thing protecting a
sandbox's `process` and `proxy` handles: `Shutdown` can be tearing down the same
microVM as the claim holder. So `teardownRunningVM` takes both handles off the
state in the same critical section that bumps the generation, and every other
read and write of them holds `r.mu` too. Whoever takes the handles owns the stop
and the kill, and the loser finds nil and repeats only `cleanupHost`, which is
written to be repeatable.

An activation is the other side of that: `Shutdown` decides whether to kill a
microVM by reading handles a create or wake has not published yet, so it can find
nothing to tear down, delete the sandbox and hand the slot back while the guest is
still coming up. `activateSandboxVM` therefore rechecks after publishing each
handle whether the sandbox is still its own, and fails with `errActivationAbandoned`
if not. The recheck comes *after* the publication on purpose: the caller's rollback
is what kills the microVM, and a rollback only tears down handles it can see.
Carrying on instead would finish building a guest nothing tracks, holding the netns
and jail mounts of a slot already handed to someone else — and outliving the runner,
since jailer's children are their own process group. Today a stale `loadSnapshot`
usually fails that activation anyway, because `Shutdown` deletes the data directory
out from under it, but cold-boot recovery boots the rootfs and reads no snapshot
files, so that accident stops covering it.

A claim comes with a budget: `beginTransition` returns the context its operation
must run under, which is the caller's detached from cancellation and capped at two
minutes (create claims in `reserveSandbox` instead and gets three, since it clones
the rootfs and snapshot before booting). Returning the two together is deliberate —
a new lifecycle operation cannot acquire a claim without also bounding it.

That budget starts when the claim is won. Waiting for another operation to finish
is bounded separately and more generously, by `transitionWaitBudget`, so that
queueing behind a slow create neither eats the time the waiter needs for its own
host work nor makes it give up on a claim that is still guaranteed to be released.

Detaching from cancellation means a client that disconnects mid-delete cannot
abandon a sandbox with its host resources half torn down. The cap is what
guarantees the claim is eventually released: the host commands and Firecracker API
calls inherit that context, so `exec.CommandContext` kills a wedged command when it
expires. Without it a single stuck `umount` would hold the sandbox forever, because
the callers that drive these operations have no deadline of their own — the
control-plane RPCs carry the API request context and the idle sweeper's context
lives until the API process exits.

The budget is a ceiling rather than a replacement, so a caller that asks for less
time keeps its own deadline; the admission canary relies on that to bound its
cleanup. The budgets are compile-time values rather than environment variables:
they exist to cap pathological cases, not as tuning knobs.

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
  the golden-snapshot build; see `scripts/firecracker.ee/create-golden-snapshot.sh`).
- **Disk** — capped by the ext4 image size of the golden template
  (`rootfs.ext4`; default `FIRECRACKER_ROOTFS_SIZE_MB=2048`, see
  `build-rootfs-template.sh` / golden-build `MANIFEST.json`). Each
  sandbox gets a sparse copy of that image at create time. There is no
  `SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB` equivalent; ENOSPC occurs when the
  guest fills that ext4 device.

To change limits in production, rebuild the golden snapshot/rootfs on the host
rather than tuning runner env vars.

## Boot parameters (`boot.json`)

`create-golden-snapshot.sh` writes `boot.json` into its `--out` directory next to
`snapshot_mem` and `snapshot_state`, recording what it actually sent to the
Firecracker API:

```json
{
  "schema_version": 1,
  "vcpu_count": 1,
  "mem_size_mib": 512,
  "kernel_image_path": "/vmlinux",
  "boot_args": "console=ttyS0 reboot=k panic=1 pci=off ipv6.disable=1 init=/sandbox-daemon ip=172.16.0.10::172.16.0.1:255.255.255.0::eth0:off",
  "rootfs_drive_path": "/rootfs.ext4",
  "guest_mac": "AA:FC:00:00:00:01",
  "guest_ip": "172.16.0.10",
  "host_tap_device_name": "fc-tap-0",
  "daemon_port": 8081,
  "created_at": "2026-08-20T11:35:58Z"
}
```

Paths are as Firecracker sees them inside the jail. Most of these values exist
nowhere else on the runner: `vcpu_count` and `mem_size_mib` come from the create
script's `VCPUS`/`MEM_MIB` environment variables and have no `Config` field, and
`boot_args` is assembled by the script. That is harmless while the runner only
restores the snapshot, but a runner that has to boot a replacement VM for a dead
guest would otherwise have to invent them. Recording them at build time keeps
that boot faithful to how the snapshot was built, and keeps each sandbox pinned
to its own snapshot lineage once a host serves more than one flavour.

Admission rejects a snapshot whose `guest_ip`, `host_tap_device_name`,
`daemon_port` or `boot_args` gateway contradicts the runner: those are baked into
the guest or the restored device model, so a mismatch yields sandboxes that never
answer instead of ones that fail visibly. The gateway is the third field of the
kernel `ip=` parameter and must equal the host address in
`SANDBOX_RUNNER_FIRECRACKER_HOST_TAP_IP_CIDR`. The guest applied it at boot, so
it is part of the snapshotted routing table; a tap address in the same subnet
still passes the admission canary while leaving every sandbox without a route off
the tap.

A missing `boot.json` means an incomplete snapshot, not a sidecar to reconstruct
from current config — the three files have to describe one build. Admission
rebuilds the whole set when `SANDBOX_RUNNER_FIRECRACKER_CREATE_SNAPSHOT_SCRIPT`
is set, and otherwise fails naming the script to re-run. Bump `schema_version`
when the layout changes; the runner rejects versions it does not know rather
than guessing.

A set that is already complete is verified rather than trusted, because the
sidecar only certifies the snapshot the runner actually restores if the configured
paths lead to it. Regeneration replaces `snapshot_mem` and `snapshot_state` inside
the `--out` directory, so a configured path that is an independent copy or a
symlink out of it keeps serving the previous snapshot under a `boot.json`
describing the new one, and survives every restart in that state. So whenever the
runner owns snapshot creation and its outputs are present, admission resolves both
configured paths and fails if either does not land on the generated file. Hosts
without a create script, or whose `--out` directory holds no generated files, have
nothing to compare against and are left alone.

## Guest death

A microVM can die on its own: the guest daemon runs as PID 1 with `panic=1
reboot=k` in the boot args, so killing it panics the kernel, which resets the CPU
and exits the VMM. A host `SIGKILL` of the VMM and a `reboot` from inside the
guest end the same way. Because jailer execs Firecracker in place, the process
the runner starts lives exactly as long as the guest, so waiting on it is the
detection: `startCommand` reports the exit and `handleGuestDeath` reacts.

Telling that apart from the runner's own kills is what `sandboxState.generation`
is for. `teardownRunningVM` bumps it before killing the process, and each exit
callback carries the generation of the microVM it was registered for, so a stop,
delete, wake rollback or shutdown is not read as a crash — and an old
incarnation's exit arriving late cannot mark a freshly woken one dead. The death
is recorded straight away, before the handler queues for the sandbox's transition
claim: an exit that arrives while another operation holds the sandbox is only
looked at once that operation has released it, by which point its teardown has
bumped the generation past the one the exit carries. A wake that fails *because*
its guest just died goes exactly that way. The teardown that follows is still
gated on the generation, so it cannot run twice on the same microVM.

A dead guest is torn down immediately and **hands its slot back**, which is what
bounds the damage: a crashed sandbox costs disk and nothing else, so a client
retrying cannot accumulate slots. What is left behind is what an idle stop leaves
behind, minus a usable snapshot — stopped, no slot, files intact — so the
existing paths need no special case: `StopSandbox` is idempotent instead of
looping on a dead API socket, the idle sweeper can delete it, and `DaemonURL`
reports it not running.

It is **not** restored from its snapshot afterwards. The guest kept writing to
the rootfs after that snapshot was taken, and restoring a memory image whose
cached filesystem metadata no longer matches the disk corrupts it silently —
nothing detects the mismatch. So `EnsureSandboxRunning` refuses a pinned
(`mustColdBoot`) sandbox. Bringing it back needs a boot of its existing rootfs,
which this runtime does not do yet; until then a request for a crashed sandbox
fails and the sandbox stays deletable.

The pin is set by the restore, not by the death, and specifically *before* the
restore is asked for rather than after it reports success. `snapshot/load` carries
`resume_vm: true`, so one request both loads the snapshot and lets the guest start
writing to a rootfs that snapshot no longer describes. Its failures are therefore
ambiguous — a deadline that expires while the response is read leaves no way to know
whether the guest ran — and the ambiguous case is resolved as if it did. Only the
snapshot `StopSandbox` takes of the paused guest clears the pin.

Tying it to the restore is what makes it hold for a guest that dies mid-wake, where
the death cannot be told apart from the rollback's own kill, and for a wake that
failed after the restore for a reason of its own: that guest ran too, so its snapshot
is just as stale. The cost of pinning first is that a load which failed without
resuming anything is pinned as well, which costs a cold boot instead of a restore.
Placing the pin immediately before the request rather than earlier is what keeps that
cost small — every step ahead of it fails unambiguously with no guest having run, so
those failures leave the sandbox wakeable. Until cold boot exists a pinned sandbox is
refused, which is the deliberate trade: a loud failure instead of a silently
corrupted disk.

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
