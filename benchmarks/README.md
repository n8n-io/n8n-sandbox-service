# Benchmarks

Load tests for the n8n Sandbox Service using [k6](https://grafana.com/docs/k6/).

## Setup

1. Install k6: https://grafana.com/docs/k6/latest/set-up/install-k6/
2. Start the sandbox service (e.g. `make up` from the repo root).

## Scripts

| Script | Description |
|--------|-------------|
| `k6-sandbox-lifecycle.js` | Full sandbox lifecycle: create, execute `echo 'hello'`, optionally wake, delete. |

## Running

Smoke test (single iteration):

```sh
k6 run --vus 1 --iterations 1 benchmarks/k6-sandbox-lifecycle.js
```

Load run (`SCENARIO=load`, the default: ramp to 50 VUs over 30s, hold 1m, ramp down):

```sh
k6 run benchmarks/k6-sandbox-lifecycle.js
```

Baseline run (one operation at a time, which is what [docs/performance.md](../docs/performance.md) records):

```sh
k6 run -e SCENARIO=baseline -e ITERATIONS=30 benchmarks/k6-sandbox-lifecycle.js
```

## Measuring wake

`WAKE_AFTER` idles between the first and second exec so the API's idle sweeper
stops the sandbox, which turns the second exec into a wake. The sandbox only
stops if the wait exceeds `SANDBOX_API_IDLE_STOP_AFTER` (default 1h) plus up to
one `SANDBOX_API_IDLE_SWEEP_INTERVAL` (default 1m), so set both low on the
environment under test:

```sh
k6 run -e SCENARIO=baseline -e WAKE_AFTER=8 benchmarks/k6-sandbox-lifecycle.js
```

With `SANDBOX_API_IDLE_STOP_AFTER=3s` and `SANDBOX_API_IDLE_SWEEP_INTERVAL=2s`,
a `WAKE_AFTER=8` wait reliably lands after a sweep.

## Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `BASE_URL` | `http://127.0.0.1:8080` | Sandbox service API base URL |
| `API_KEY` | `test` | API key for `X-Api-Key` header |
| `SCENARIO` | `load` | `load` for the ramped run, `baseline` for one operation at a time |
| `ITERATIONS` | `30` | Iterations in the baseline scenario |
| `WAKE_AFTER` | `0` | Seconds to idle before the second exec; `0` skips the wake step |

Example:

```sh
k6 run -e BASE_URL=http://10.0.0.5:8080 -e API_KEY=my-key benchmarks/k6-sandbox-lifecycle.js
```

## Custom metrics

Each script reports per-step latency trends in addition to k6's built-in HTTP metrics:

- `sandbox_create_duration` — time to create a sandbox
- `sandbox_exec_duration` — time to execute a command and receive the full response
- `sandbox_wake_exec_duration` — same, for the exec that wakes a stopped sandbox (only with `WAKE_AFTER`)
- `sandbox_delete_duration` — time to delete a sandbox

Client-side numbers include network latency to the API. To attribute time inside
the service, join them with the server-side events described in
[docs/observability.md](../docs/observability.md).
