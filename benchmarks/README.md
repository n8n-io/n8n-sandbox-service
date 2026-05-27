# Benchmarks

Load tests for the n8n Sandbox Service using [k6](https://grafana.com/docs/k6/).

## Setup

1. Install k6: https://grafana.com/docs/k6/latest/set-up/install-k6/
2. Start the sandbox service (e.g. `make up` from the repo root).

## Scripts

| Script | Description |
|--------|-------------|
| `k6-sandbox-lifecycle.js` | Full sandbox lifecycle: create, execute `echo 'hello'`, delete. Ramps to 50 VUs. |

## Running

Smoke test (single iteration):

```sh
k6 run --vus 1 --iterations 1 benchmarks/k6-sandbox-lifecycle.js
```

Full run (default stages: ramp to 50 VUs over 30s, hold 1m, ramp down):

```sh
k6 run benchmarks/k6-sandbox-lifecycle.js
```

## Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `BASE_URL` | `http://127.0.0.1:8080` | Sandbox service API base URL |
| `API_KEY` | `test` | API key for `X-Api-Key` header |

Example:

```sh
k6 run -e BASE_URL=http://10.0.0.5:8080 -e API_KEY=my-key benchmarks/k6-sandbox-lifecycle.js
```

## Custom metrics

Each script reports per-step latency trends in addition to k6's built-in HTTP metrics:

- `sandbox_create_duration` — time to create a sandbox
- `sandbox_exec_duration` — time to execute a command and receive the full response
- `sandbox_delete_duration` — time to delete a sandbox
