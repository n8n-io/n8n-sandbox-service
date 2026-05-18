# Configuration

All services are configured via environment variables.

## Contents

- [API](#api)
- [Runner](#runner)
- [Sandbox daemon](#sandbox-daemon)

## API

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

**Heartbeat grace:** Runners stay in the in-memory registry while their gRPC stream is open. Between heartbeats, the API still considers a runner usable for new placements only if its last heartbeat was within `SANDBOX_API_RUNNER_HEARTBEAT_GRACE`. After that window, the runner is skipped until the next heartbeat (or dropped when the stream ends). Tune this if heartbeats are infrequent or the network is slow, so runners are not marked stale too aggressively.

## Runner

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

## Sandbox daemon

These variables are set inside each sandbox container and are typically baked into the sandbox image or passed through the runner.

| Variable | Default | Description |
|---|---|---|
| `SANDBOX_EXEC_MAX_EVENT_BYTES` | `16777216` | Max bytes of event history retained per execution (16 MiB) |
| `SANDBOX_EXEC_RETAIN` | `10m` | Duration to retain completed executions (Go [`time.ParseDuration`](https://pkg.go.dev/time#ParseDuration) syntax, e.g. `10m`, `1h`) |
