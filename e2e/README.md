# End-to-end tests

Playwright drives the HTTP API. Shell scripts start Docker networks, the API, and one or more **sandbox runners** (Docker-in-Docker `n8n-sandbox-runner` containers).

Run with `e2e/run-all.sh`.

`e2e/run.sh` sets short `SANDBOX_API_IDLE_*` values on the API container so `tests/sandbox-idle-ttl.spec.ts` can assert idle stop, wake, and delete.
