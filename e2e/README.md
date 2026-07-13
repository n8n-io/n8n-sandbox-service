# End-to-end tests

Playwright drives the HTTP API. Shell scripts start Docker networks, the API, and one or more sandbox runners (Docker-in-Docker `n8n-sandbox-service-runner-dind` containers).

Run with `e2e/run-all.sh` (SQLite API by default, plus a required Postgres API phase at the end).

Idle TTL: The default `e2e/run.sh` API uses production defaults for `SANDBOX_API_IDLE_*`. Run **`e2e/run-idle-ttl.sh`** for a dedicated stack with short idle timers and only `tests/sandbox-idle-ttl.spec.ts` (also used as phase 4 of `run-all.sh`).

Postgres API: Phase 5 of `run-all.sh` runs **`e2e/run-postgres.sh`** (idle TTL, two-runner placement, and multi-pod API specs against Postgres with the Docker runner). Run it alone with `e2e/run-postgres.sh`.

## Backend tags

Specs run on every runner lane by default. Apply a marker
from `tests/tags.ts` only to a spec (or `describe`) that is backend-specific:

- `DOCKER_ONLY` (`@docker-only`) — e.g. inner-container recovery, xfs disk quota.
- `FIRECRACKER_ONLY` (`@firecracker-only`) — e.g. rootfs capacity checks.

Each lane excludes the other lane's marker via `--grep-invert`: the Docker lane
(`e2e/run.sh`) skips `@firecracker-only`, and the Firecracker lane
(`e2e/run-firecracker.sh`) skips `@docker-only`. A new untagged spec therefore
runs on both backends automatically.

From a local machine, run the full Azure Firecracker flow with:

```bash
RESOURCE_GROUP=my-resource-group bash e2e/run-firecracker-azure.sh
```

The wrapper provisions the VM, runs the Firecracker e2e tests over SSH, collects
logs on failure, and destroys the VM resources on exit.

Idle TTL (Firecracker): Like Docker, the default `e2e/run-firecracker.sh` uses
production `SANDBOX_API_IDLE_*` defaults and excludes `tests/sandbox-idle-ttl.spec.ts`.
Run `e2e/run-firecracker-idle-ttl.sh` for a dedicated stack with short idle
timers and only that spec (uses its own HTTP/gRPC/control ports so it can run
back-to-back with the main suite on the same VM).

Two runners (Firecracker): Firecracker runners cannot share one host network
namespace, so two-runner placement/resilience tests use a control VM (API +
runner 1) and a peer VM (runner 2). Provision with `E2E_PEER_VM_ENABLED=true`
and run `e2e/run-firecracker-two-runners-azure.sh` (or the full
`e2e/run-firecracker-azure.sh` flow, which includes this phase).

`e2e/run-firecracker.sh` starts the runner on `127.0.0.1:18082` and starts
per-sandbox Firecracker daemon proxies at `18100` by default. Keep those port
ranges separate when overriding `RUNNER_ADDR` or `FIRECRACKER_PROXY_PORT_START`.
