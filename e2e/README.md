# End-to-end tests

Playwright drives the HTTP API. Shell scripts start Docker networks, the API, and one or more runners.

## Docker lane

`e2e/run-all.sh` builds the images and SDK once, then runs five phases:

| Phase | Script | Covers |
| --- | --- | --- |
| 1 | `run-no-runner.sh` | API only |
| 2 | `run-two-runners.sh` | Placement and resilience across two runners |
| 3 | `run.sh` | Full suite, single runner (excludes idle specs) |
| 4 | `run-idle-ttl.sh` | `sandbox-idle-ttl.spec.ts` and `sandbox-ephemeral.spec.ts` on a dedicated stack with short `SANDBOX_API_IDLE_*` timers |
| 5 | `run-postgres.sh` | Idle TTL, two-runner placement and multi-pod API specs against Postgres |

Each script also runs alone. `e2e/run-postgres-multi-pod.sh` fronts two API pods with an nginx gRPC TCP proxy (like a k8s Service) and runs the `@multi-pod-failover` specs, which stop the lead pod and assert runner re-registration and idle sweeping on the survivor.

## Firecracker lane

Needs a Linux host with KVM; from a local machine, `e2e/run-firecracker-azure.sh` provisions an Azure VM, runs the suite over SSH, collects logs on failure and destroys the VM on exit:

```bash
RESOURCE_GROUP=my-resource-group bash e2e/run-firecracker-azure.sh
```

On the VM:

- `run-firecracker.sh` — full suite (excludes idle specs). Runner on `127.0.0.1:18082`, per-sandbox daemon proxies from `18100`; keep those ranges apart when overriding `RUNNER_ADDR` or `FIRECRACKER_PROXY_PORT_START`.
- `run-firecracker-idle-ttl.sh` — idle specs on a dedicated stack with its own ports, so it can run back-to-back with the main suite.
- `run-firecracker-two-runners-azure.sh` — Firecracker runners cannot share a host network namespace, so two-runner specs use a control VM (API + runner 1) and a peer VM (runner 2). Provision with `E2E_PEER_VM_ENABLED=true`; the full Azure flow includes this phase.

## Backend tags

Specs run on both lanes by default. Tag only backend-specific specs (or `describe` blocks) with a marker from `tests/tags.ts`:

- `DOCKER_ONLY` (`@docker-only`) — e.g. inner-container recovery, capability policy, xfs disk quota.
- `FIRECRACKER_ONLY` (`@firecracker-only`) — e.g. rootfs capacity checks.

Each lane excludes the other's marker via `--grep-invert`.
