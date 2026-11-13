# Firecracker runner runtime

Each sandbox is a Firecracker microVM restored from a golden snapshot built on the host. Upstream Firecracker and jailer; one runner process per host (netns/veth names `fc-sb-{n}`, `fc-veth-{n}` would collide otherwise). Host setup is in [quickstart-firecracker-linux.md](../../../../docs/quickstart-firecracker-linux.md); env vars in [configuration.md](../../../../docs/configuration.md#firecracker-runner-backend-config).

## Admission (`Prepare` / `Ready`)

The runner reports `Healthy=false`, fails `/readyz` and `Ready()` until all of these pass, retrying with backoff:

1. Host NAT — IPv4 forwarding plus idempotent MASQUERADE/FORWARD rules.
2. Guest assets — jailer, firecracker, `template/rootfs.ext4`, `template/vmlinux`, and optionally `MANIFEST.json` / expected `git_sha` / daemon checksum. `vmlinux` must be owned by root, readable by all and writable by no other user, as `build-rootfs-template.sh` installs it. That is checked here, again after step 3 and before every cold boot.
3. Golden snapshot — if `snapshot_mem`, `snapshot_state` or `boot.json` is missing and `SANDBOX_RUNNER_FIRECRACKER_CREATE_SNAPSHOT_SCRIPT` is set, run `create-golden-snapshot.sh` locally (snapshots are not portable across CPU generations). The runner passes its own `GUEST_IP`, `HOST_TAP_IP_CIDR`, `HOST_TAP_DEVICE_NAME` and `DAEMON_PORT`; `MEM_MIB`, `VCPUS` and `VOLUME_DRIVES` are script-owned.
4. Snapshot pin — mem/state exist and `boot.json` agrees with the runner (see [boot.json](../../../../docs/configuration.md#golden-snapshot-boot-parameters-bootjson)).
5. Canary — create `admission-canary-*`, probe `/healthz`, run a tiny exec and a files round-trip, delete it. A canary whose cleanup fails keeps its slot and fails admission, so a capacity-1 runner is never healthy with a leaked slot.

## Networking

Each sandbox slot gets its own network namespace:

| Piece | Role |
|-------|------|
| TAP `fc-tap-0` | Virtio NIC; gateway `172.16.0.1`, guest `172.16.0.10` (baked into the snapshot) |
| veth `fc-uplink` (netns) ↔ `fc-veth-{slot}` (host) | Routes guest traffic to the host routing table |
| Host proxy `127.0.0.1:{port}` | Runner listens on the host, dials the guest daemon from inside the netns on an OS thread that is discarded afterwards |

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

`network/network.go` owns topology (netns, TAP, veth, routes, NAT); `network/egress.go` owns the `FORWARD` rules for the `netpolicy` denylist shared with Docker and the egress `none` rule, a `FORWARD -i <tap> -j DROP` inserted ahead of them. Guest IPv6 is disabled via `ipv6.disable=1` in the snapshot boot args, `sysctl.user.max_user_namespaces=0` refuses user namespaces as Docker's default seccomp profile does, and `pci=off` keeps virtio on MMIO (why: [docs/security-model.md](../../../../docs/security-model.md), "Guest to host and network"); changing boot args requires a snapshot rebuild.

## Slots and lifecycle

A slot ties one netns, TAP, veth and proxy port together. Slots are in-memory, runner-local and not stable across restarts: a stopped sandbox releases its slot and may wake onto another.

Nothing in the namespace build depends on the sandbox that will use it, so a background wirer (`network_wirer.go`, started from `Prepare`) builds every free slot at startup and rebuilds each slot after release. Activation skips the build on a wired slot (`setup_network_ms` ≈ 0), waits on one mid-build, and builds inline on one the wirer has not reached. Readiness does not wait for wiring, and teardown still deletes the namespace, so every sandbox gets a fresh one. `Shutdown` clears the namespaces of free slots, best-effort; startup reconcile sweeps whatever that leaves. `sandbox_slots_unwired` reports how many slots are not built; zero in steady state. The egress `none` DROP is the one sandbox-specific rule, so activation adds it on top of the built namespace, on create and on every wake; pre-wired namespaces only ever carry the default policy.

Invariants (rationale in the code comments of `stop_wake.go`, `crash.go`, `network_wirer.go`, `runtime.go`):

- Create, stop, wake and delete are mutually exclusive per sandbox (`beginTransition`). Each claim runs under a fixed budget detached from the caller's cancellation, so a disconnected client or wedged host command cannot leave a sandbox half torn down. `Shutdown` is the one operation that does not wait.
- A stop releases its slot even if host cleanup fails; the sandbox becomes an ordinary stopped one. A failed delete keeps its slot because the API retries deletes every sweep; a failed create releases it because nothing will ever retry.
- Teardown marks its slot unwired, and slot setup deletes leftover netns/veth before creating them, so a slot handed back with leftovers comes up clean.
- A teardown never touches a slot another sandbox holds: `clearSlotNetwork` runs under the slot's network lock and re-reads the owner there, per incarnation, because `Shutdown` can release a slot from under a concurrent teardown while the next sandbox takes it. A stopped sandbox has no slot by the time it is deleted, so only its jail is cleaned.
- Every microVM incarnation has a `generation`; an exit callback carrying an old generation is ignored, which is how runner-initiated kills are told apart from guest deaths.
- The guest daemon holds root, and serves only `/healthz` and `POST /volumes/mount`, until the first `POST /volumes/mount` ([security-model.md](../../../../docs/security-model.md#guest-to-host-and-network)). Create and cold boot send it after `probe_daemon`, with no volumes, and fail on any answer but `204`, `404` or `501`. A wake from a stop snapshot doesn't: its daemon dropped root before the snapshot.

## Guest death and cold boot

The guest daemon is PID 1 with `panic=1 reboot=k`, and jailer execs Firecracker in place, so a guest dying ends the process the runner is waiting on. The runner tears the sandbox down and releases its slot, leaving it stopped with files intact. It is never restored from its now-stale snapshot (a memory image whose cached filesystem metadata predates later disk writes corrupts the rootfs silently); instead the next request cold boots the existing rootfs with the boot parameters recorded in `boot.json`, and is answered with `409 sandbox_restarted` ([API.md](../../../../docs/API.md#http-409-sandbox_restarted--the-sandbox-came-back-without-its-memory)). `boot.go` replays the Firecracker API calls `create-golden-snapshot.sh` uses. Behavior and metrics: [architecture.md](../../../../docs/architecture.md#recovering-a-crashed-guest), [observability.md](../../../../docs/observability.md#runner-guest-deaths).

## Guest clock

The guest runs no NTP. A restored guest would resume with the wall clock it had when its snapshot was taken, so every restore (create and wake) sends `clock_realtime: true`, which makes KVM advance the guest's kvm-clock by the time since then; the guest's monotonic clock jumps with it. That only works while the guest keeps time with kvm-clock, so the golden snapshot boots with `clocksource=kvm-clock`: where the guest sees an invariant TSC (Azure v6 sizes, for example), the kernel would otherwise switch to `tsc`, which resumes at snapshot time. Firecracker supports this on x86_64 only, and refuses the restore unless KVM recorded the wall-clock time in the snapshot, which needs host kernel 5.16 or later and a TSC-based host clocksource (`tsc`, or Hyper-V's TSC page on Azure). A host that never records it fails the admission canary. A stop snapshot taken while KVM could not record it makes that wake fail; the sandbox is then pinned to cold boot like after a failed restore.

## Volume drives

A volume drive is a read-only drive the golden snapshot reserves for a volume, a read-only filesystem for the sandbox. Firecracker can't add drives to a restored VM, so the snapshot attaches `VOLUME_DRIVES` of them up front (default 8: `vol0`–`vol7`, right after the rootfs, seen by the guest as `/dev/vdb`–`/dev/vdi`). Each is empty: every jail hard-links them to one root-owned read-only file, `volume-drive-placeholder` in the jailer base dir. A cold boot attaches them again.

## Volume builder

`SandboxControl.BuildVolume` turns a tar into an ext4 image for a read-only volume (`volume_build.go`). The API streams a header (tar length, entry count, content bytes and decompressed length, as it counted them under the volume tar policy) and the tar; the runner streams back the image size, the image and its SHA-256. Nothing in the API calls it yet.

A build runs in a builder microVM, one per runner at a time, without a sandbox slot:

1. Refuse, before reading any tar byte and as a capacity refusal the API can retry elsewhere, while the runner is cordoned, not ready or already building, or if the tar plus the image would take the data volume below the 5% floor.
2. Write the tar to `SANDBOX_RUNNER_DATA_DIR/volume-build/`, with a sparse output file sized from the entry count and content bytes (the rule is in `internal/daemon/volumebuild/size.go`), a 4 KiB status file and a clone of the template rootfs.
3. Replay the clone's journal with `e2fsck -E journal_only`: the golden snapshot's microVM left the template mounted read-write, and the kernel does not mount such a file system from a read-only drive. The template itself is never touched.
4. Make the tar and the clone root-owned `0444` and bind them, and the template kernel, read-only into the jail `volume-build` before jailer starts; the output and status files are bound writable. The jail gets the empty network namespace `fc-volume-build`.
5. Cold boot with the golden snapshot's vCPUs, memory and kernel command line from `boot.json`, so the builder follows the snapshot without a second copy of those values. Two things differ: no network interface and no `ip=` (which stalls a boot without one), and the build parameters, which reach the daemon as environment. Drives in order: rootfs, tar, output, status.
6. The daemon, as init in build mode (`internal/daemon/volumebuild`), formats the output drive as ext4 without a journal, unpacks the tar into it up to the counts in the header, checks it with `e2fsck -fn`, writes the status record and reboots, which ends Firecracker.
7. Read the status record strictly and send the image as opaque bytes. A builder still running after `SANDBOX_RUNNER_FIRECRACKER_VOLUME_BUILD_TIMEOUT` (default `5m`), or whose caller went away, is killed.

A failed build ends the call with one reason: `tar`, `too_large`, `no_space`, `format`, `mount`, `fsck`, `internal` or `timeout`. Jail, binds, namespace and files are removed on every way out, at `Shutdown`, and by startup reconcile. The builder's console is not kept. The runner host needs `e2fsck`, the sandbox image `mke2fs` and `e2fsck`. The builder boots the daemon that `create-golden-snapshot.sh` installed into the template: one from before build mode starts as a normal daemon there, and the build ends with `timeout`. Metrics: [observability.md](../../../../docs/observability.md#metrics); trust: [security-model.md](../../../../docs/security-model.md#volume-builder).

## Resource limits

CPU, memory and disk are fixed when the golden snapshot and `rootfs.ext4` are built, not by runner env vars: `VCPUS`/`MEM_MIB` in `create-golden-snapshot.sh`, `FIRECRACKER_ROOTFS_SIZE_MB` (default 2048) in `build-rootfs-template.sh`. Each sandbox gets a sparse copy of the rootfs; filling it yields ENOSPC in the guest. Rebuild on the host to change limits.

## Data volume

Each sandbox keeps its rootfs and memory snapshot under `SANDBOX_RUNNER_DATA_DIR` until it is deleted. On XFS with reflink, the rootfs shares blocks with the template and the memory snapshot with the golden snapshot, so a new sandbox takes almost no space; it grows as the guest writes and when a stop saves its memory. The checks are in `disk.go`:

- **Create:** refused while the volume has less than 5% free, like when no slot is free, so the API tries another runner.
- **Stop:** needs room for the memory snapshot, or it fails before pausing the guest; the sandbox keeps running and the idle sweeper retries. If the snapshot fails after the pause, the guest is resumed, or shut down like a crashed guest if that fails too.
- **Wake:** not checked; the sandbox already holds its space.

The runner never deletes a sandbox to make room. Sandboxes already on the volume can still fill the last 5%; their writes then fail.

Setup:

- Put `SANDBOX_RUNNER_DATA_DIR`, the template and the golden snapshot on one dedicated XFS filesystem with `reflink=1`.
- Use a directory inside that filesystem, not its mount point. The runner does not create it, so an unmounted volume leaves the runner unready instead of filling the wrong disk.

## Limitations

- Sandboxes are not reattached after a runner restart; startup reconcile removes orphaned data directories, jail state and slot namespaces (a sandbox's jail directory still holding an active bind mount is logged and left in place).
- Egress policy is per-netns iptables, not nftables sets; no per-sandbox allowlists.
