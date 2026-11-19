# Docker/Sysbox Runner Runtime

This runtime starts each sandbox as a Docker container managed by the runner's
inner Docker daemon. In production it is expected to run in a Sysbox-backed
runner container so Docker-in-Docker can run without giving ordinary workload
containers direct access to the host Docker daemon.

## Technology

- Uses both the Docker REST API and the Docker CLI against
  `SANDBOX_RUNNER_DOCKER_HOST`, which must be a `unix://` socket.
- Starts sandbox containers from `SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE`.
- Connects containers to a runner bridge network by egress mode:
  `runner-bridge` for `public`, `runner-no-egress` for `none` (a Docker
  `--internal` network; its bridge device is `br-no-egress`, the name being
  over the 15-character interface limit). The host must have `br_netfilter`
  loaded with `bridge-nf-call-iptables=1`, or bridged sandbox traffic bypasses
  the iptables rules. The runner refuses to start unless the sysctl reads `1`
  in its network namespace; before kernel 5.3 the sysctl is host-wide, and when
  it is absent from that namespace the runner requires
  `SANDBOX_RUNNER_DOCKER_ACK_BR_NETFILTER_IS_LOADED=true` instead.
  `scripts/setup-sysbox.sh` loads the module; the sysctl defaults to `1` once
  it is loaded. The check runs only at startup.
- Disables IPv6 in every container, unless the host booted with
  `ipv6.disable=1` (read from `/sys/module/ipv6/parameters/disable` at
  startup). IPv6 as a kernel module must be loaded before sandboxes start.
- Proxies API traffic to the sandbox daemon on port `8081`.

## Supported Features

- Pulls the sandbox image in the background and retries with backoff until it is
  available.
- Reports readiness only after the sandbox image is present and Docker is
  reachable.
- Reports capacity from the current managed container count.
- Applies default memory, CPU, PID, and optional disk quota limits on create.
- Drops every Linux capability, restores none, and sets `no-new-privileges`.
  The container runs as uid 1000, so it holds nothing effective; the empty
  bounding set stops it gaining anything, and `no-new-privileges` makes any
  setuid binary inert. With an image that ships no `sudo`, that leaves no path
  to root and so no `apt-get`; packages install unprivileged under `/home/user`.
  Sysbox isolation and privileged local DinD apply to the runner container, so
  every sandbox container gets the same policy. Never create a sandbox container
  with `--privileged`: Docker then ignores `--cap-drop` and the policy has no
  effect.
- Applies network isolation rules through `netrules`, keyed on the bridge
  interface: on `runner-bridge` a drop on traffic between containers and the
  `netpolicy` denylist, a plain `DOCKER-USER` DROP on `br-no-egress`, and the
  host block on both.
- Points container DNS at `SANDBOX_RUNNER_DOCKER_DNS_SERVERS` when set, else
  leaves Docker's default ([Sandbox DNS](../../../../docs/security-model.md#sandbox-dns)).
- Waits for daemon `/healthz` and a tiny `/executions` round trip before
  returning a sandbox as ready.
- Wakes stopped containers on proxy access, reapplies network rules, and waits
  for the daemon before proxying.
- Uses singleflight so concurrent wake requests for the same sandbox only run
  one wake operation.
- Runs a delete within `TransitionBudget`, without the caller's cancellation or
  deadline.
- Best-effort reconciles and removes stale managed containers on startup and
  shutdown. A container with volumes that survives that fails the startup
  ([Volumes](#volumes)).
- Detects guests that died on their own and reports the restart to the client.
- Attaches read-only volumes on create ([Volumes](#volumes)).

## Crash recovery

Containers run with `--restart unless-stopped`, so Docker restarts a dead container itself — possibly on a new IP the network policy does not know about, and without telling the client its memory is gone. `crash.go` closes both gaps: it watches `docker events` for `die` on the runner's containers, excuses deaths the runner itself caused (recorded before the stop/remove and matched against the event; exit codes are ignored), and marks everything else as a crash. A marked sandbox reports not running until the wake path reapplies its policy and waits for its daemon, which turns the restart into `WakeResult{Recovered}` and `409 sandbox_restarted` ([API.md](../../../../docs/API.md#http-409-sandbox_restarted--the-sandbox-came-back-without-its-memory)). The watcher reconnects if the stream drops; a death missed meanwhile is served without its `409`. Rationale for the mark bookkeeping is in the `crash.go` comments; the cross-backend behavior is in [architecture.md](../../../../docs/architecture.md#recovering-a-crashed-guest).

An idle stop loses memory the same way (`docker stop` then `docker start` re-runs the entrypoint on the same writable layer; there is no snapshot to resume from), but reports no `409`: the stop is already visible as `status: stopped`.

## Volumes

The create options name the volumes, `tar` only. A create naming another kind
fails with `FailedPrecondition` before anything is acquired or created; one
that would bind a volume while the volume cache can't be made `nosuid,nodev`
fails the same way, after the acquire and before the container exists. A
sandbox without volumes takes none of these steps.

- **Cache:** the volume cache (`internal/runner/volumecache`) downloads a tar,
  checks its size and sha256, and unpacks it once, holding it to the counts the
  create options carry (entries, file content, decompressed length) under the
  volume tar rules, through an `os.Root`. It keeps only the tree: directories
  `0755`, files `0644` or `0755`, symbolic links with any target, all owned by
  the runner. The tree takes room for the tar and its files until it is
  complete, and counts what it takes on disk afterwards. At most two unpacks
  run at once; the wait for one and the unpack share the fetch timeout with the
  download. A failed or cut-off unpack leaves nothing; the tar is deleted either
  way. Measuring, eviction and the startup wipe never follow a link.
- **Create:** the cache pins by sandbox ID, so a create with volumes holds a
  lock on the ID, which refuses a second create of the ID and makes a delete
  of it wait, and is refused while a container of the ID exists. Every volume is acquired at once, pinned for the sandbox, before
  the container exists, waiting until a minute before the create's deadline
  (half the time left if that is more). A required volume that fails fails the
  create; a capacity refusal from the cache stays one. An optional one is left
  out for `cache_full`, `timeout`, `download` or `unpack`. The container gets
  `--mount type=bind,source=<tree>,target=<mount path>,readonly` per volume it
  has, and the label `sandbox-service.volumes=true`. Docker creates a missing
  mount target, root-owned, below the image's root-owned `/mnt`.
- **Flags:** Docker can't set `nosuid` or `nodev` on a bind mount, but a bind
  copies the flags of the mount its source is on. Before the first bind, the
  runner bind-mounts the cache dir onto itself and remounts it with both,
  keeping the flags it had; a mount that has them already is left alone. Each
  tree must be a runner-owned `0755` directory on a `nosuid,nodev` mount, or
  the create fails, whatever the volume.
- **Stop, wake, restart after a crash:** the container keeps its mounts and the
  sandbox its pins; Docker binds the same trees again.
- **Delete, failed create:** the pins are released once the container is gone,
  also by a delete that finds none.
  A container a failed cleanup left behind keeps them until a delete removes
  it. Errors that can reach the client name the cache dir only as
  `<volume cache>`.
- **Runner restart:** startup reconcile removes every managed container and the
  volume cache wipes its dir. A container with volumes still there after a
  second attempt fails the startup, since the runner would serve it without its
  trees.

The cache dir must be the same path for the runner and `dockerd`, which holds
for the runner image, where both run in one container: a `dockerd` elsewhere
(`SANDBOX_RUNNER_DOCKER_HOST`) can't bind the trees.
