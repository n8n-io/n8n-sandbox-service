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
- Drops every Linux capability, then restores only `CAP_AUDIT_WRITE`,
  `CAP_CHOWN`, `CAP_DAC_OVERRIDE`, `CAP_FOWNER`, `CAP_SETGID`, and `CAP_SETUID`.
  This keeps passwordless `sudo` usable for common package installation while
  preventing network administration, mounts, tracing, device creation, raw
  sockets, and other privileged operations. Sysbox isolation and privileged
  local DinD apply to the runner container, so every sandbox container gets the
  same policy. Never create a sandbox container with `--privileged`: Docker then
  ignores `--cap-drop` and the allowlist has no effect.
- Applies Docker-specific network isolation rules through `netrules`.
- Waits for daemon `/healthz` and a tiny `/executions` round trip before
  returning a sandbox as ready.
- Wakes stopped containers on proxy access, reapplies network rules, and waits
  for the daemon before proxying.
- Uses singleflight so concurrent wake requests for the same sandbox only run
  one wake operation.
- Best-effort reconciles and removes stale managed containers on startup and
  shutdown.
