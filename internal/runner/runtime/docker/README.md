# Docker/Sysbox Runner Runtime

This runtime starts each sandbox as a Docker container managed by the runner's
inner Docker daemon. In production it is expected to run in a Sysbox-backed
runner container so Docker-in-Docker can run without giving ordinary workload
containers direct access to the host Docker daemon.

## Technology

- Uses the Docker CLI against `SANDBOX_RUNNER_DOCKER_HOST`.
- Starts sandbox containers from `SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE`.
- Connects containers to the runner bridge network.
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

Sandbox containers run with `--restart unless-stopped`, so Docker brings a died
container back on its own — with its files, and without telling anyone. Two things
are wrong with that: the container can come back on a different IP its network policy
does not know about, and the client is never told that everything the sandbox held in
memory is gone.

`crash.go` closes both. The runner subscribes to `docker events` filtered to `die` on
its own containers and reads every death it did not ask for as a crash. Deaths it did
ask for are recorded before the stop or remove that causes them and matched against
the event; exit codes are never consulted, because a guest that exits `0` on its own
has still lost what it was running, and a `docker stop` of a healthy sandbox produces
the non-zero exit of a SIGTERM'd daemon.

A recorded stop only excuses a death if the stop it was recorded for happened. One
that reports failure is dropped at once, because the container is still running and
its next death is a crash; one that reports success but never produces a death — a
container already exited when it was removed — expires instead. Both matter: a mark
outliving its reason is what would excuse a real crash of the same container, serving
it with no `409` and with network rules still naming the address it had.

The stop that cancels a pending restart records nothing at all, because the crash it
follows already emitted the container's one death. That is decided when the stop is
issued rather than cleaned up afterwards: a death reaches the runner through the
`docker events` subprocess, so a mark can still be waiting on its event while the
next request wakes the sandbox, and nothing later in the wake path can tell that mark
apart from one whose death is never coming.

`DaemonURL` then reports the sandbox as not running until the wake path has
re-admitted it: reapplied its network policy for the address it came back on, and
waited for its daemon. That is what forces a container which already looks healthy
through that path, and what turns the restart into `WakeResult{Recovered}` and from
there into `409 sandbox_restarted`, exactly as on the Firecracker runner.

Losing the event stream is silent — containers keep working while crashes stop being
reported — so the watcher reconnects for the life of the runner. A death missed while
it was down means a restart served without its `409`.

An idle stop costs a sandbox the same memory a crash does, because this runtime has
nothing to resume from: `StopSandboxContainer` is a `docker stop` and the wake is a
`docker start`, which re-runs the entrypoint on the same writable layer. Firecracker
differs here — its idle stop snapshots the paused guest — so only that runtime has a
genuinely transparent wake. This one reports no `409` for it anyway, because an idle
stop is already visible to the client as `status: stopped`, and the `409` is for the
loss with no other signal. See
[API.md](../../../../docs/API.md#http-409-sandbox_restarted--the-sandbox-came-back-without-its-memory).
