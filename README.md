# n8n Sandbox Service

The n8n Sandbox Service provides isolated execution environments via a REST API. Each sandbox is a Debian-based Docker container managed by an in-container Docker daemon, with a per-sandbox HTTP daemon that handles exec and file operations.

## Runtime Model

- The service image runs on `docker:29.3.1-dind-alpine3.23`.
- The outer service container is expected to run with `sysbox-runc`.
- Sandboxes are started from a separate Debian sandbox image referenced by `SANDBOX_DOCKER_SANDBOX_IMAGE`.
- The service talks to sandbox daemons over the inner Docker bridge on port `8081`.

## Quick Start

Build both images:

```bash
make docker-amd64
```

Run locally:

```bash
./scripts/run-locally.sh
```

Verify the service:

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

| Variable | Default | Description |
|---|---|---|
| `SANDBOX_API_KEYS` | *(required)* | Comma-separated list of valid API keys |
| `SANDBOX_DOCKER_SANDBOX_IMAGE` | *(required)* | Docker image used for sandbox containers |
| `SANDBOX_DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker daemon endpoint used by the service |
| `SANDBOX_LISTEN_ADDR` | `:8080` | HTTP listen address |
| `SANDBOX_DATA_DIR` | `/var/sandboxes` | Directory for SQLite state |
| `SANDBOX_IDLE_TTL_SECONDS` | `3600` | Seconds of inactivity before a sandbox is reaped |
| `SANDBOX_MAX_FILE_BYTES` | `10485760` | Maximum file upload size (10 MB) |
| `SANDBOX_ENABLE_CGROUPS` | `true` | Whether Docker resource limits are applied |
| `SANDBOX_INTER_SANDBOX_NETWORK_ENABLED` | `false` | Whether sandboxes may talk to each other on `runner-bridge` |

## Development

Run unit tests:

```bash
make test
```

Run e2e tests against the Docker-backed runtime:

```bash
./e2e/run.sh
```
