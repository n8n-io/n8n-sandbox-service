# End-to-end tests

Playwright drives the HTTP API. Shell scripts start Docker networks, the API, and one or more **sandbox runners** (Docker-in-Docker `n8n-sandbox-runner` containers).

Run with `e2e/run-all.sh`.

**Idle TTL:** The default `e2e/run.sh` API uses production defaults for `SANDBOX_API_IDLE_*`. Run **`e2e/run-idle-ttl.sh`** for a dedicated stack with short idle timers and only `tests/sandbox-idle-ttl.spec.ts` (also used as phase 4 of `run-all.sh`).
