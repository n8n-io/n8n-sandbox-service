# Configuration

All services are configured via environment variables.

## Contents

- [API](#api)
- [Runner](#runner)
- [Sandbox daemon](#sandbox-daemon)
- [Metrics](#metrics)
- [Disk quotas](#disk-quotas)

## API

| Variable | Default | Description |
| --- | --- | --- |
| `SANDBOX_API_KEYS` | *(required)* | Comma-separated **admin** API keys. Full access to all sandboxes and `/admin/tenants` key management. Self-hosted can use these alone without minting tenant keys. |
| `SANDBOX_API_RUNNER_REGISTRATION_TOKEN` | *(required)* | Shared secret; runners authenticate to the private gRPC registration service with `Authorization: Bearer …` |
| `SANDBOX_API_RUNNER_API_KEY` | *(empty)* | Optional API key injected by the API when calling runner HTTP |
| `SANDBOX_API_LOG_LEVEL` | `info` | Minimum log severity (`debug`, `info`, `warn`, `error`; case-insensitive) |
| `SANDBOX_API_LISTEN_ADDR` | `:8080` | Public HTTP listen address |
| `SANDBOX_API_GRPC_LISTEN_ADDR` | `:9090` | Private gRPC listen address for runner registration streams |
| `SANDBOX_API_STORE` | `sqlite` | Store backend: `sqlite` (default, single API pod) or `postgres` (multi-pod) |
| `SANDBOX_API_DATA_DIR` | `/var/lib/n8n-sandbox-api` | SQLite store directory when `SANDBOX_API_STORE=sqlite`; must exist and be writable. Mount a persistent volume here to retain sandbox state across API pod restarts. |
| `SANDBOX_API_POSTGRES_HOST` | *(required with postgres)* | Postgres host |
| `SANDBOX_API_POSTGRES_PORT` | `5432` | Postgres port |
| `SANDBOX_API_POSTGRES_USER` | *(required with postgres)* | Postgres user |
| `SANDBOX_API_POSTGRES_PASSWORD` | *(required with postgres)* | Postgres password |
| `SANDBOX_API_POSTGRES_DB` | *(required with postgres)* | Postgres database name |
| `SANDBOX_API_POSTGRES_SSLMODE` | `require` | Postgres TLS mode (`disable`, `require`, `verify-full`, etc.) |
| `SANDBOX_API_MAX_FILE_BYTES` | `10485760` | Maximum file upload size (10 MB) |
| `SANDBOX_API_ENABLE_CORS` | `false` | Enable CORS headers (allow all origins); needed for the browser playground |
| `SANDBOX_API_METRICS_ENABLED` | `false` | When true, expose Prometheus `/metrics` on the public listener (no `X-Api-Key`; firewall the port). See [Metrics](#metrics). |
| `SANDBOX_API_RUNNER_HEARTBEAT_GRACE` | `45s` | How long after the last gRPC heartbeat a runner remains eligible for placement (Go [`time.ParseDuration`](https://pkg.go.dev/time#ParseDuration) syntax, e.g. `45s`, `2m`) |
| `SANDBOX_API_ORPHAN_REAP_BUFFER` | `5m` | How long after a runner deregisters before the idle sweeper removes its orphaned sandbox rows from the store |
| `SANDBOX_API_GRPC_TLS_CERT_FILE` | *(required)* | Server certificate (PEM) for the registration gRPC listener |
| `SANDBOX_API_GRPC_TLS_KEY_FILE` | *(required)* | Server private key (PEM) |
| `SANDBOX_API_GRPC_TLS_CLIENT_CA_FILE` | *(required)* | CA bundle (PEM) that signed runner client certificates |
| `SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CA_FILE` | *(required)* | CA (PEM) that signed runner **SandboxControl** server certs |
| `SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CERT_FILE` | *(required)* | API client certificate (PEM) for SandboxControl |
| `SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_KEY_FILE` | *(required)* | API client key (PEM) |
| `SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_SERVER_NAME` | *(empty)* | TLS verify name when it must differ from the dial host (defaults to the runner host) |

**Heartbeat grace:** Runners stay registered while their gRPC stream is open and heartbeats are written to the store (Postgres) or in-memory registry (SQLite). Between heartbeats, the API still considers a runner usable for new placements only if its last heartbeat was within `SANDBOX_API_RUNNER_HEARTBEAT_GRACE`. After that window, the runner is skipped until the next heartbeat.

**Multi-pod (Postgres):** Set `SANDBOX_API_STORE=postgres` and the `SANDBOX_API_POSTGRES_*` variables when running multiple API replicas. Sandbox metadata and runner heartbeats are shared in Postgres; the idle sweeper uses a Postgres advisory lock so only one pod sweeps at a time. New sandboxes are placed on the eligible runner with the lowest reported `capacity_used`. Disable `api.persistence` in Helm when using Postgres (state lives in the database, not local disk).

The idle sweeper waits `SANDBOX_API_ORPHAN_REAP_BUFFER` (default `5m`) after a runner's last heartbeat before removing its orphaned sandbox rows from the API store (sandboxes that are already idle stop/delete candidates). With SQLite, this is based on observing the runner stream close; with Postgres, it is based on `last_seen` in the shared registry.

## Runner

### Shared runner config

| Variable | Default | Description |
| --- | --- | --- |
| `SANDBOX_RUNNER_API_KEYS` | *(required)* | Comma-separated list of valid internal API keys accepted from the API container |
| `SANDBOX_RUNNER_LOG_LEVEL` | `info` | Minimum log severity (`debug`, `info`, `warn`, `error`; case-insensitive) |
| `SANDBOX_RUNNER_LISTEN_ADDR` | `:8080` | HTTP listen address |
| `SANDBOX_RUNNER_API_GRPC_ADDR` | *(required)* | API `host:port` for gRPC registration |
| `SANDBOX_RUNNER_REGISTRATION_TOKEN` | *(required)* | Must match `SANDBOX_API_RUNNER_REGISTRATION_TOKEN` on the API |
| `SANDBOX_RUNNER_HTTP_BASE_URL` | *(required)* | Base URL the API uses to reach this runner (e.g. `http://runner:8080`) |
| `SANDBOX_RUNNER_ID` | hostname | Stable runner id sent to the API |
| `SANDBOX_RUNNER_CAPACITY_TOTAL` | `1000` | Reported capacity for placement (`0` = unlimited) |
| `SANDBOX_RUNNER_DATA_DIR` | `/var/sandboxes` | Directory for SQLite state |
| `SANDBOX_RUNNER_IDLE_TTL_SECONDS` | `3600` | Seconds of inactivity before a sandbox is reaped |
| `SANDBOX_RUNNER_METRICS_ENABLED` | `false` | When true, expose Prometheus `/metrics` on the runner's HTTP listener (no `X-Api-Key`; firewall the port). See [Metrics](#metrics). |
| `SANDBOX_RUNNER_INTER_SANDBOX_NETWORK_ENABLED` | `false` | Whether sandboxes may talk to each other on `runner-bridge` |
| `SANDBOX_RUNNER_REGISTRATION_GRPC_CA_FILE` | *(required)* | CA (PEM) that signed the API registration gRPC server cert |
| `SANDBOX_RUNNER_REGISTRATION_GRPC_CERT_FILE` | *(required)* | Runner client cert (PEM) for registration mTLS |
| `SANDBOX_RUNNER_REGISTRATION_GRPC_KEY_FILE` | *(required)* | Runner client key (PEM) for registration mTLS |
| `SANDBOX_RUNNER_REGISTRATION_GRPC_SERVER_NAME` | *(empty)* | TLS name to verify on the API registration cert (defaults to host from `SANDBOX_RUNNER_API_GRPC_ADDR`) |
| `SANDBOX_RUNNER_CONTROL_GRPC_LISTEN_ADDR` | `:9091` | Listen address for **SandboxControl** gRPC |
| `SANDBOX_RUNNER_CONTROL_GRPC_ADVERTISE_ADDR` | *(derived)* | `host:port` sent to the API in heartbeats; required if listen is set and `SANDBOX_RUNNER_HTTP_BASE_URL` cannot be used to derive host/port |
| `SANDBOX_RUNNER_CONTROL_GRPC_TLS_CERT_FILE` | *(required)* | Server cert (PEM) for SandboxControl |
| `SANDBOX_RUNNER_CONTROL_GRPC_TLS_KEY_FILE` | *(required)* | Server private key (PEM) |
| `SANDBOX_RUNNER_CONTROL_GRPC_TLS_CLIENT_CA_FILE` | *(required)* | CA (PEM) that signed API client certificates for SandboxControl |

### Docker runner backend config

These variables are parsed by the Docker/sysbox runner entrypoint.

| Variable | Default | Description |
| --- | --- | --- |
| `SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE` | *(required)* | Docker image used for sandbox containers |
| `SANDBOX_RUNNER_DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker daemon endpoint used by the runner |
| `SANDBOX_RUNNER_ENABLE_CGROUPS` | `true` | Whether Docker resource limits are applied |
| `SANDBOX_RUNNER_DEFAULT_MEMORY_MB` | `512` | Default Docker memory limit per sandbox in megabytes |
| `SANDBOX_RUNNER_DEFAULT_CPU_PERCENT` | `100` | Default Docker CPU limit as a percentage of one core |
| `SANDBOX_RUNNER_DEFAULT_PIDS_MAX` | `256` | Default Docker process count limit per sandbox |
| `SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB` | `0` | Per-sandbox writable-layer quota in MB (`--storage-opt size=`). Effective only when the storage pool mounts successfully — see [Disk quotas](#disk-quotas). `0` means no quota. |
| `SANDBOX_RUNNER_DISK_QUOTA_POOL_SIZE_GB` | *(derived)* | Size of the xfs+prjquota storage pool backing the inner dockerd. Defaults to `ceil(SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB × SANDBOX_RUNNER_CAPACITY_TOTAL × 1.2 / 1024)` (per-sandbox quota times runner capacity, plus 20% headroom for sandbox image layers). Set explicitly to override. |
| `SANDBOX_RUNNER_DOCKER_INSECURE_REGISTRIES` | *(empty)* | Comma-separated insecure registries passed to dockerd |

### Firecracker runner backend config

These variables are parsed by the Firecracker runner entrypoint.

#### Firecracker slots

A Firecracker slot is one schedulable VM position on a runner. The runner creates slots from `SANDBOX_RUNNER_CAPACITY_TOTAL`; if capacity is `100`, the runner has slots `0` through `99`. A sandbox occupies one slot from create until stop/delete.

Slots give the host-side Firecracker resources stable names without exposing those details to the API or clients. For slot `n`, the runtime derives:

- Network namespace: `fc-sb-n`
- TAP name inside the namespace: `SANDBOX_RUNNER_FIRECRACKER_HOST_TAP_DEVICE_NAME`
- Host-local daemon proxy port: `SANDBOX_RUNNER_FIRECRACKER_PROXY_PORT_START + n`

| Variable | Default | Description |
| --- | --- | --- |
| `SANDBOX_RUNNER_FIRECRACKER_JAILER_BIN` | `/opt/firecracker/bin/jailer` | Path to the Firecracker jailer binary |
| `SANDBOX_RUNNER_FIRECRACKER_BIN` | `/opt/firecracker/bin/firecracker` | Path to the Firecracker VMM binary |
| `SANDBOX_RUNNER_FIRECRACKER_JAILER_BASE_DIR` | `/srv/jailer` | Base directory passed to `jailer --chroot-base-dir` |
| `SANDBOX_RUNNER_FIRECRACKER_TEMPLATE_DIR` | `/srv/firecracker/template` | Directory containing the snapshot rootfs (`rootfs.ext4`) |
| `SANDBOX_RUNNER_FIRECRACKER_SNAPSHOT_MEM_PATH` | `/srv/firecracker/snapshots/mem` | Host path bind-mounted into the jail as `/snapshot_mem` |
| `SANDBOX_RUNNER_FIRECRACKER_SNAPSHOT_STATE_PATH` | `/srv/firecracker/snapshots/state` | Host path bind-mounted into the jail as `/snapshot_state` |
| `SANDBOX_RUNNER_FIRECRACKER_SNAPSHOT_VIRTIO_BLOCK_PATH` | `/rootfs.ext4` | Rootfs path expected by the snapshot metadata |
| `SANDBOX_RUNNER_FIRECRACKER_GUEST_IP` | `172.16.0.10` | Guest IP expected by the restored snapshot |
| `SANDBOX_RUNNER_FIRECRACKER_HOST_TAP_DEVICE_NAME` | `fc-tap-0` | TAP device name inside each sandbox netns |
| `SANDBOX_RUNNER_FIRECRACKER_HOST_TAP_IP_CIDR` | `172.16.0.1/24` | Host-side TAP address inside each sandbox netns |
| `SANDBOX_RUNNER_FIRECRACKER_DAEMON_PORT` | `8081` | Sandbox daemon port inside the guest |
| `SANDBOX_RUNNER_FIRECRACKER_PROXY_LISTEN_IP` | `127.0.0.1` | Host-side listen IP for daemon proxies |
| `SANDBOX_RUNNER_FIRECRACKER_PROXY_PORT_START` | `18081` | First host-side proxy port. Slot `n` uses `PROXY_PORT_START+n`. |
| `SANDBOX_RUNNER_FIRECRACKER_SOCKET_WAIT_ATTEMPTS` | `120` | Number of checks while waiting for `firecracker.socket` |
| `SANDBOX_RUNNER_FIRECRACKER_SOCKET_WAIT_INTERVAL_MS` | `20` | Delay between Firecracker socket checks in milliseconds |
| `SANDBOX_RUNNER_FIRECRACKER_DAEMON_WAIT_TIMEOUT` | `60s` | Maximum time to wait for guest daemon health after snapshot restore |

#### Resource limits

The Firecracker runner does **not** read `SANDBOX_RUNNER_DEFAULT_MEMORY_MB`,
`SANDBOX_RUNNER_DEFAULT_CPU_PERCENT`, or `SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB`.
CPU and memory are fixed in the golden memory snapshot; disk capacity is fixed
by the template `rootfs.ext4` size. Change those by rebuilding the host snapshot
assets (see [`internal/runner/runtime/firecracker.ee/README.md`](../internal/runner/runtime/firecracker.ee/README.md)).

## Sandbox daemon

These variables are set inside each sandbox container and are typically baked into the sandbox image or passed through the runner.

| Variable | Default | Description |
| --- | --- | --- |
| `SANDBOX_DAEMON_LOG_LEVEL` | `info` | Minimum log severity (`debug`, `info`, `warn`, `error`; case-insensitive) |
| `SANDBOX_EXEC_MAX_EVENT_BYTES` | `16777216` | Max bytes of event history retained per execution (16 MiB) |
| `SANDBOX_EXEC_RETAIN` | `10m` | Duration to retain completed executions (Go [`time.ParseDuration`](https://pkg.go.dev/time#ParseDuration) syntax, e.g. `10m`, `1h`) |

## Metrics

The API and runner can each expose a Prometheus `/metrics` endpoint on the same HTTP port that serves their public API. Set `SANDBOX_API_METRICS_ENABLED=true` and/or `SANDBOX_RUNNER_METRICS_ENABLED=true` to enable. The endpoint:

- Bypasses `X-Api-Key`, matching the n8n core operator model. Operators are expected to firewall the HTTP port or front it with a private LB; otherwise anyone reaching the listener can read the metrics.
- Uses the `sandbox_` namespace, with a `role` label (`api` or `runner`) on every metric so series from both binaries can live in one Prometheus.
- Bounds cardinality by labeling HTTP series with the route pattern (e.g. `/sandboxes/{id}/executions`), not the raw path.

Series exposed today:

- `sandbox_http_requests_total{role,route,method,status}` and `sandbox_http_request_duration_seconds{role,route,method}` (both binaries).
- API: `sandbox_sandbox_operations_total{operation,result}`, `sandbox_sandboxes_active`, `sandbox_runners_registered`.
- Runner: `sandbox_container_operations_total{operation,result}`, `sandbox_container_operation_duration_seconds{operation}`, `sandbox_containers_active`.
- Plus the standard `go_*` and `process_*` collectors.

## Disk quotas

When `SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB > 0`, the runner emits `--storage-opt size=Nm` on each sandbox so the inner dockerd caps that sandbox's writable layer. To make the flag enforce anything, `scripts/start-runner.sh` allocates a loopback xfs image (sized from `SANDBOX_RUNNER_DISK_QUOTA_POOL_SIZE_GB` — see the table above for how the default is derived), formats it, mounts it with `prjquota` at `/var/lib/docker`, and starts the inner dockerd with `--storage-driver=overlay2` against that mount. When `SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB` is unset/`0`, the pool is not created and dockerd uses its default storage with no per-sandbox enforcement.

**Host kernel requirement:** the runner container's host kernel must be built with `CONFIG_XFS_QUOTA` (=y or =m). Every mainstream Linux distro kernel ships with this enabled. The notable exception is **Docker Desktop's linuxkit kernel** on macOS, which omits it — on that host the loopback mount fails and the runner logs `disk quota enforcement: DISABLED` and continues without per-sandbox enforcement. Sandboxes still run, just without writable-layer caps. To check a node: `zcat /proc/config.gz | grep XFS_QUOTA` or `cat /boot/config-$(uname -r) | grep XFS_QUOTA`.
