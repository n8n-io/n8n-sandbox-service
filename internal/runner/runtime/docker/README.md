# Docker/Sysbox Runner Runtime

This runtime starts each sandbox as a Docker container managed by the runner's
inner Docker daemon. In production it is expected to run in a Sysbox-backed
runner container so Docker-in-Docker can run without giving ordinary workload
containers direct access to the host Docker daemon.

## Technology

- Uses the Docker CLI against `SANDBOX_RUNNER_DOCKER_HOST`.
- Starts sandbox containers from `SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE`.
- Connects containers to the runner bridge network. The host must have
  `br_netfilter` loaded with `bridge-nf-call-iptables=1`, or bridged sandbox
  traffic bypasses the iptables rules. `scripts/setup-sysbox.sh` loads the
  module; the sysctl defaults to `1` once it is loaded, so only a host that
  overrides it needs attention.
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
- Applies Docker-specific network isolation rules through `netrules`.
- Waits for daemon `/healthz` and a tiny `/executions` round trip before
  returning a sandbox as ready.
- Wakes stopped containers on proxy access, reapplies network rules, and waits
  for the daemon before proxying.
- Uses singleflight so concurrent wake requests for the same sandbox only run
  one wake operation.
- Best-effort reconciles and removes stale managed containers on startup and
  shutdown.
- Detects guests that died on their own and reports the restart to the client.

## Crash recovery

Containers run with `--restart unless-stopped`, so Docker restarts a dead container itself — possibly on a new IP the network policy does not know about, and without telling the client its memory is gone. `crash.go` closes both gaps: it watches `docker events` for `die` on the runner's containers, excuses deaths the runner itself caused (recorded before the stop/remove and matched against the event; exit codes are ignored), and marks everything else as a crash. A marked sandbox reports not running until the wake path reapplies its policy and waits for its daemon, which turns the restart into `WakeResult{Recovered}` and `409 sandbox_restarted` ([API.md](../../../../docs/API.md#http-409-sandbox_restarted--the-sandbox-came-back-without-its-memory)). The watcher reconnects if the stream drops; a death missed meanwhile is served without its `409`. Rationale for the mark bookkeeping is in the `crash.go` comments; the cross-backend behavior is in [architecture.md](../../../../docs/architecture.md#recovering-a-crashed-guest).

An idle stop loses memory the same way (`docker stop` then `docker start` re-runs the entrypoint on the same writable layer; there is no snapshot to resume from), but reports no `409`: the stop is already visible as `status: stopped`.
