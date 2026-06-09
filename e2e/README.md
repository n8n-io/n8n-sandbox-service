# End-to-end tests

Playwright drives the HTTP API. Shell scripts start Docker networks, the API, and one or more **sandbox runners** (Docker-in-Docker `n8n-sandbox-service-runner-dind` containers).

Run with `e2e/run-all.sh`.

**Idle TTL:** The default `e2e/run.sh` API uses production defaults for `SANDBOX_API_IDLE_*`. Run **`e2e/run-idle-ttl.sh`** for a dedicated stack with short idle timers and only `tests/sandbox-idle-ttl.spec.ts` (also used as phase 4 of `run-all.sh`).

## Backend tags

Specs use two Playwright backend tags:

- `@docker-runner` for the Docker/Sysbox runner lane.
- `@firecracker-runner` for the Firecracker runner lane.

Specs should import `RUNNER_TAGS` or `BOTH_RUNNERS` from `tests/tags.ts`
instead of spelling tag strings inline. Tests compatible with both runners use
`BOTH_RUNNERS`, which applies both runtime tags. Firecracker e2e runs use
`e2e/run-firecracker.sh`, which starts the API and Firecracker runner as host
processes on the prepared VM and selects `@firecracker-runner`.

From a local machine, run the full Azure Firecracker flow with:

```bash
RESOURCE_GROUP=my-resource-group bash e2e/run-firecracker-azure.sh
```

The wrapper provisions the VM, runs the Firecracker e2e tests over SSH, collects
logs on failure, and destroys the VM resources on exit.

`e2e/run-firecracker.sh` starts the runner on `127.0.0.1:18082` and starts
per-sandbox Firecracker daemon proxies at `18100` by default. Keep those port
ranges separate when overriding `RUNNER_ADDR` or `FIRECRACKER_PROXY_PORT_START`.
