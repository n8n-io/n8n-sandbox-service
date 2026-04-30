# n8n Sandbox Service

The n8n Sandbox Service provides isolated execution environments via a REST API. Each sandbox is a Debian-based Docker container managed by an in-container Docker daemon, with a per-sandbox HTTP daemon that handles exec and file operations.

## Runtime Model

- The public API runs in a dedicated `n8n-sandbox-api` container.
- One or more `n8n-sandbox-runner` containers run Docker-in-Docker and manage sandbox lifecycles (the local script starts two so you can exercise round-robin placement).
- The runner container is expected to run with `sysbox-runc`.
- Sandboxes are started from a separate Debian sandbox image referenced by `SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE`.
- The API forwards sandbox and image requests to the runner; the runner talks to sandbox daemons over the inner Docker bridge on port `8081`.

## Quick Start

Build all images:

```bash
make docker-amd64
```

Run locally (starts the API plus **two** runners on a shared Docker network, both registered over gRPC):

```bash
./scripts/run-locally.sh
```

Verify the API:

```bash
curl http://localhost:8080/healthz
```

## API Usage

All endpoints except `/healthz` require `X-Api-Key`.

### Create a sandbox

```bash
curl -s -X POST http://localhost:8080/sandboxes \
  -H "X-Api-Key: test" | jq
```

### Run a command

```bash
curl -s -X POST http://localhost:8080/sandboxes/<id>/exec \
  -H "X-Api-Key: test" \
  -H "Content-Type: application/json" \
  -d '{"command": "echo hello world"}'
```

### Write a file

```bash
curl -s -X PUT "http://localhost:8080/sandboxes/<id>/files?path=/tmp/hello.txt" \
  -H "X-Api-Key: test" \
  --data-binary "file contents here"
```

### Read a file

```bash
curl -s "http://localhost:8080/sandboxes/<id>/files/content?path=/tmp/hello.txt" \
  -H "X-Api-Key: test"
```

### List a directory

```bash
curl -s "http://localhost:8080/sandboxes/<id>/files?path=/tmp" \
  -H "X-Api-Key: test" | jq
```

### Delete a file

```bash
curl -s -X DELETE "http://localhost:8080/sandboxes/<id>/files?path=/tmp/hello.txt" \
  -H "X-Api-Key: test"
```

### Delete a sandbox

```bash
curl -s -X DELETE http://localhost:8080/sandboxes/<id> \
  -H "X-Api-Key: test"
```

## Configuration

### API container

| Variable | Default | Description |
|---|---|---|
| `SANDBOX_API_KEYS` | *(required)* | Comma-separated list of valid external API keys |
| `SANDBOX_API_RUNNER_REGISTRATION_TOKEN` | *(required)* | Shared secret; runners authenticate to the private gRPC registration service with `Authorization: Bearer …` |
| `SANDBOX_API_RUNNER_API_KEY` | *(empty)* | Optional API key injected by the API when calling runner HTTP |
| `SANDBOX_API_LISTEN_ADDR` | `:8080` | Public HTTP listen address |
| `SANDBOX_API_GRPC_LISTEN_ADDR` | `:9090` | Private gRPC listen address for runner registration streams |
| `SANDBOX_API_DATA_DIR` | `/tmp/sandbox-api` | SQLite store directory |
| `SANDBOX_API_MAX_FILE_BYTES` | `10485760` | Maximum file upload size (10 MB) |
| `SANDBOX_API_RUNNER_HEARTBEAT_GRACE` | `45s` | How long after the last gRPC heartbeat a runner remains eligible for placement (Go [`time.ParseDuration`](https://pkg.go.dev/time#ParseDuration) syntax, e.g. `45s`, `2m`) |

Runners register over gRPC and report health and capacity; the API picks a runner (round-robin) when creating sandboxes and stores that runner’s HTTP base URL for later proxying.

**Heartbeat grace:** Runners stay in the in-memory registry while their gRPC stream is open. Between heartbeats, the API still considers a runner usable for new placements only if its last heartbeat was within `SANDBOX_API_RUNNER_HEARTBEAT_GRACE`. After that window, the runner is skipped until the next heartbeat (or dropped when the stream ends). Tune this if heartbeats are infrequent or the network is slow, so runners are not marked stale too aggressively.

### Runner container

| Variable | Default | Description |
|---|---|---|
| `SANDBOX_RUNNER_API_KEYS` | *(required)* | Comma-separated list of valid internal API keys accepted from the API container |
| `SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE` | *(required)* | Docker image used for sandbox containers |
| `SANDBOX_RUNNER_DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker daemon endpoint used by the runner |
| `SANDBOX_RUNNER_LISTEN_ADDR` | `:8080` | HTTP listen address |
| `SANDBOX_RUNNER_API_GRPC_ADDR` | *(empty)* | API `host:port` for gRPC registration (omit to disable registration) |
| `SANDBOX_RUNNER_REGISTRATION_TOKEN` | *(empty)* | Must match `SANDBOX_API_RUNNER_REGISTRATION_TOKEN` on the API when `SANDBOX_RUNNER_API_GRPC_ADDR` is set |
| `SANDBOX_RUNNER_HTTP_BASE_URL` | *(empty)* | Base URL the API uses to reach this runner (e.g. `http://runner:8080`); required when registering |
| `SANDBOX_RUNNER_ID` | hostname | Stable runner id sent to the API |
| `SANDBOX_RUNNER_CAPACITY_TOTAL` | `1000` | Reported capacity for placement (`0` = unlimited) |
| `SANDBOX_RUNNER_DATA_DIR` | `/var/sandboxes` | Directory for SQLite state |
| `SANDBOX_RUNNER_IDLE_TTL_SECONDS` | `3600` | Seconds of inactivity before a sandbox is reaped |
| `SANDBOX_RUNNER_ENABLE_CGROUPS` | `true` | Whether Docker resource limits are applied |
| `SANDBOX_RUNNER_INTER_SANDBOX_NETWORK_ENABLED` | `false` | Whether sandboxes may talk to each other on `runner-bridge` |
| `SANDBOX_RUNNER_DOCKER_INSECURE_REGISTRIES` | *(empty)* | Comma-separated insecure registries passed to dockerd |

## Development

Run unit tests:

```bash
make test
```

Run the full e2e suite (all topologies sequentially: no runner → two runners → single runner + full Playwright):

```bash
./e2e/run-all.sh
```

Run only one topology or the default single-runner suite:

```bash
./e2e/run.sh              # single runner + full Playwright suite
./e2e/run-no-runner.sh    # API only — expects POST /sandboxes to return 503
./e2e/run-two-runners.sh # two runners — placement routing per sandbox
```

Extra Playwright arguments can be passed to any script (for example `./e2e/run-all.sh --grep pattern`).

`run-all.sh` runs **`make docker-local` once**; the per-topology scripts skip rebuilding when invoked from it (`E2E_SKIP_BUILD=1`). To rebuild before each phase, run the topology scripts individually instead.

See **[e2e/README.md](e2e/README.md)** for how scripts map to specs, what a Playwright “worker” is, and how `resilience.spec.ts` uses the host `docker` CLI.
