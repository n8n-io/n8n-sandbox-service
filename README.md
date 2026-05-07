# n8n Sandbox Service

The n8n Sandbox Service provides isolated execution environments via a REST API. Each sandbox is a Debian-based Docker container managed by an in-container Docker daemon, with a per-sandbox HTTP daemon that handles exec and file operations.

## Runtime Model

- The public API runs in a dedicated `n8n-sandbox-api` container.
- One or more `n8n-sandbox-runner` containers run Docker-in-Docker and manage sandbox lifecycles (the local script starts two so you can exercise round-robin placement).
- The runner container is expected to run with `sysbox-runc`.
- Sandboxes are started from a separate Debian sandbox image referenced by `SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE`.
- The API forwards sandbox and image requests to the runner; the runner talks to sandbox daemons over the inner Docker bridge on port `8081`.

## Failure Behavior

- API restarts: sandbox IDs remain valid. Once API is back, existing sandboxes continue working.
- Runner stops/dies: sandboxes on that runner become unavailable. When a runner returns, previously assigned sandboxes are not guaranteed to be recoverable and should be treated as lost.
- Sandbox container exits (for example OOM): Docker restart policy restarts it; the same sandbox ID remains on the same runner.

## Quick Start

Build all images:

```bash
make docker-amd64
```

Run locally with Docker Compose (API plus two runners on a shared network). `make up` runs `scripts/bootstrap-local-mtls.sh` once to populate `.tls/` (gitignored), then starts Compose with required mTLS for registration and control gRPC. See [Runner registration gRPC (mTLS)](#runner-registration-grpc-mtls).

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
| `SANDBOX_API_ENABLE_CORS` | `false` | Enable CORS headers (allow all origins); needed for the browser playground |
| `SANDBOX_API_RUNNER_HEARTBEAT_GRACE` | `45s` | How long after the last gRPC heartbeat a runner remains eligible for placement (Go [`time.ParseDuration`](https://pkg.go.dev/time#ParseDuration) syntax, e.g. `45s`, `2m`) |
| `SANDBOX_API_GRPC_TLS_CERT_FILE` | *(required)* | Server certificate (PEM) for the registration gRPC listener |
| `SANDBOX_API_GRPC_TLS_KEY_FILE` | *(required)* | Server private key (PEM) |
| `SANDBOX_API_GRPC_TLS_CLIENT_CA_FILE` | *(required)* | CA bundle (PEM) that signed runner client certificates |
| `SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CA_FILE` | *(required)* | CA (PEM) that signed runner **SandboxControl** server certs |
| `SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CERT_FILE` | *(required)* | API client certificate (PEM) for SandboxControl |
| `SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_KEY_FILE` | *(required)* | API client key (PEM) |
| `SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_SERVER_NAME` | *(empty)* | TLS verify name when it must differ from the dial host (defaults to the runner host) |

Runners register over gRPC and report health, capacity, and a **control gRPC address**. Sandbox create/delete are gRPC-only (`SandboxControl` on the runner). Exec/files and other proxy routes always use HTTP.

**Heartbeat grace:** Runners stay in the in-memory registry while their gRPC stream is open. Between heartbeats, the API still considers a runner usable for new placements only if its last heartbeat was within `SANDBOX_API_RUNNER_HEARTBEAT_GRACE`. After that window, the runner is skipped until the next heartbeat (or dropped when the stream ends). Tune this if heartbeats are infrequent or the network is slow, so runners are not marked stale too aggressively.

### Runner container

| Variable | Default | Description |
|---|---|---|
| `SANDBOX_RUNNER_API_KEYS` | *(required)* | Comma-separated list of valid internal API keys accepted from the API container |
| `SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE` | *(required)* | Docker image used for sandbox containers |
| `SANDBOX_RUNNER_DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker daemon endpoint used by the runner |
| `SANDBOX_RUNNER_LISTEN_ADDR` | `:8080` | HTTP listen address |
| `SANDBOX_RUNNER_API_GRPC_ADDR` | *(required)* | API `host:port` for gRPC registration |
| `SANDBOX_RUNNER_REGISTRATION_TOKEN` | *(required)* | Must match `SANDBOX_API_RUNNER_REGISTRATION_TOKEN` on the API |
| `SANDBOX_RUNNER_HTTP_BASE_URL` | *(required)* | Base URL the API uses to reach this runner (e.g. `http://runner:8080`) |
| `SANDBOX_RUNNER_ID` | hostname | Stable runner id sent to the API |
| `SANDBOX_RUNNER_CAPACITY_TOTAL` | `1000` | Reported capacity for placement (`0` = unlimited) |
| `SANDBOX_RUNNER_DATA_DIR` | `/var/sandboxes` | Directory for SQLite state |
| `SANDBOX_RUNNER_IDLE_TTL_SECONDS` | `3600` | Seconds of inactivity before a sandbox is reaped |
| `SANDBOX_RUNNER_ENABLE_CGROUPS` | `true` | Whether Docker resource limits are applied |
| `SANDBOX_RUNNER_INTER_SANDBOX_NETWORK_ENABLED` | `false` | Whether sandboxes may talk to each other on `runner-bridge` |
| `SANDBOX_RUNNER_DOCKER_INSECURE_REGISTRIES` | *(empty)* | Comma-separated insecure registries passed to dockerd |
| `SANDBOX_RUNNER_REGISTRATION_GRPC_CA_FILE` | *(required)* | CA (PEM) that signed the API registration gRPC server cert |
| `SANDBOX_RUNNER_REGISTRATION_GRPC_CERT_FILE` | *(required)* | Runner client cert (PEM) for registration mTLS |
| `SANDBOX_RUNNER_REGISTRATION_GRPC_KEY_FILE` | *(required)* | Runner client key (PEM) for registration mTLS |
| `SANDBOX_RUNNER_REGISTRATION_GRPC_SERVER_NAME` | *(empty)* | TLS name to verify on the API registration cert (defaults to host from `SANDBOX_RUNNER_API_GRPC_ADDR`) |
| `SANDBOX_RUNNER_CONTROL_GRPC_LISTEN_ADDR` | `:9091` | Listen address for **SandboxControl** gRPC |
| `SANDBOX_RUNNER_CONTROL_GRPC_ADVERTISE_ADDR` | *(derived)* | `host:port` sent to the API in heartbeats; required if listen is set and `SANDBOX_RUNNER_HTTP_BASE_URL` cannot be used to derive host/port |
| `SANDBOX_RUNNER_CONTROL_GRPC_TLS_CERT_FILE` | *(required)* | Server cert (PEM) for SandboxControl |
| `SANDBOX_RUNNER_CONTROL_GRPC_TLS_KEY_FILE` | *(required)* | Server private key (PEM) |
| `SANDBOX_RUNNER_CONTROL_GRPC_TLS_CLIENT_CA_FILE` | *(required)* | CA (PEM) that signed API client certificates for SandboxControl |

## Runner registration gRPC (mTLS)

**Local Docker Compose:** `make up` runs `scripts/bootstrap-local-mtls.sh`, which writes a private CA plus leaf certs into `.tls/` at the repo root. If those files already exist, bootstrap does nothing unless you set `SANDBOX_TLS_REGEN=1`. Compose always mounts `.tls` via `compose.tls.yaml` and sets required `SANDBOX_*_GRPC_TLS_*` variables.

**Kubernetes:** Use your own CA (often `cert-manager`). Mount PEMs from `Certificate` secrets and set the same env vars as in the tables above. See [`docs/cert-manager-k8s.md`](docs/cert-manager-k8s.md).

**Registration vs lifecycle:** Runners dial the API over gRPC for registration. Sandbox **create/delete** use the runner’s **SandboxControl** gRPC address when advertised; **exec/files** and other sandbox routes are still proxied over HTTP.

**Debugging gRPC:** See [`docs/grpcurl-debug.md`](docs/grpcurl-debug.md).

**Security FAQ (draft):** See [`docs/security-faq.md`](docs/security-faq.md).

**Weak points + hardening plan (draft):** See [`docs/security-weak-points-and-hardening.md`](docs/security-weak-points-and-hardening.md).

**Bearer token:** Still required in metadata (`Authorization: Bearer …`) in addition to mTLS for registration.

**Why this matters for trust:** mTLS ties the registration gRPC stream to a client certificate issued by your CA. A random host on the network cannot complete TLS to the API’s registry listener, so it cannot open a stream and inject a fake `runner_id` / `http_base_url` into placement. Legitimate runners still prove possession of the registration `Bearer` token in metadata. Together, only workloads you issued credentials to can show up in the runner registry. Optional mTLS on **SandboxControl** ensures only your API (presenting the control client cert) can ask a runner to create or delete sandboxes over gRPC; runners still accept the same `X-Api-Key` on that RPC as on HTTP. HTTP proxy traffic continues to use `X-Api-Key` and stored routing.

**Rotation:** The API and runner reload leaf cert/key PEMs from disk when the files change (next TLS handshake), so in Kubernetes `cert-manager` (or any process that writes renewed files into the mounted paths) can rotate leaves without restarting pods. Local bootstrap issues long‑lived dev certs; they do not auto‑renew on a timer. When they eventually expire—or whenever you want fresh material—delete `.tls/` or set `SANDBOX_TLS_REGEN=1` and run `scripts/bootstrap-local-mtls.sh` again (then restart containers or wait for the next TLS handshake so reloaded leaf material is used). Rotating the CA both sides trust means updating the mounted CA PEMs and typically rolling workloads.

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

`run-all.sh` runs `make docker-local` once. The per-topology scripts skip rebuilding when invoked from it (`E2E_SKIP_BUILD=1`). To rebuild before each phase, run the topology scripts individually instead.

See [e2e/README.md](e2e/README.md) for how scripts map to specs, what a Playwright “worker” is, and how `resilience.spec.ts` uses the host `docker` CLI.
