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
| `SANDBOX_API_KEYS` | *(required)* | Comma-separated admin API keys. Full access to all sandboxes and `/admin/tenants` key management. Self-hosted can use these alone without minting tenant keys. |
| `SANDBOX_API_PROVISIONER_KEYS` | *(empty)* | Comma-separated provisioner API keys ([security-model.md](security-model.md#provisioner-keys)). When set, startup fails if a key is also in `SANDBOX_API_KEYS` or if `SANDBOX_API_DEFAULT_MAX_SANDBOXES` is `0`. |
| `SANDBOX_API_RUNNER_REGISTRATION_TOKEN` | *(required)* | Shared secret; runners authenticate to the private gRPC registration service with `Authorization: Bearer …` |
| `SANDBOX_API_RUNNER_API_KEY` | *(empty)* | Optional API key injected by the API when calling runner HTTP; the runner also requires the client certificate above |
| `SANDBOX_API_LOG_LEVEL` | `info` | Minimum log severity (`debug`, `info`, `warn`, `error`; case-insensitive) |
| `SANDBOX_API_LISTEN_ADDR` | `:8080` | Public HTTP listen address |
| `SANDBOX_API_GRPC_LISTEN_ADDR` | `:9090` | Private gRPC listen address for runner registration streams |
| `SANDBOX_API_METRICS_LISTEN_ADDR` | *(empty)* | Dedicated `host:port` for Prometheus `/metrics`. Empty, or the same port as `SANDBOX_API_LISTEN_ADDR`, serves `/metrics` on the public listener; any other port starts a second server that serves only `/metrics`. Must not use the `SANDBOX_API_GRPC_LISTEN_ADDR` port. Ignored, with a warning, when `SANDBOX_API_METRICS_ENABLED=false`. See [Metrics](#metrics). |
| `SANDBOX_API_STORE` | `sqlite` | Store backend: `sqlite` (default, single API pod) or `postgres` (multi-pod) |
| `SANDBOX_API_DATA_DIR` | `/var/lib/n8n-sandbox-api` | SQLite store directory when `SANDBOX_API_STORE=sqlite`; must exist and be writable. Mount a persistent volume here to retain sandbox state across API pod restarts. |
| `SANDBOX_API_POSTGRES_HOST` | *(required with postgres)* | Postgres host |
| `SANDBOX_API_POSTGRES_PORT` | `5432` | Postgres port |
| `SANDBOX_API_POSTGRES_USER` | *(required with postgres)* | Postgres user |
| `SANDBOX_API_POSTGRES_PASSWORD` | *(required with postgres)* | Postgres password |
| `SANDBOX_API_POSTGRES_DB` | *(required with postgres)* | Postgres database name |
| `SANDBOX_API_POSTGRES_SSLMODE` | `require` | Postgres TLS mode (`disable`, `require`, `verify-full`, etc.) |
| `SANDBOX_API_MAX_FILE_BYTES` | `10485760` | Maximum file upload size (10 MB) |
| `SANDBOX_API_DEFAULT_MAX_SANDBOXES` | `50` | Default per-tenant sandbox quota when `POST /admin/tenants` omits `max_sandboxes` (`0` = unlimited). Must fit Postgres/SQLite `INTEGER` (`0`…`2147483647`). Soft check-then-act: concurrent creates can exceed the limit (see `docs/API.md`). |
| `SANDBOX_API_ENABLE_CORS` | `false` | Enable CORS headers (allow all origins); needed for the browser playground |
| `SANDBOX_API_METRICS_ENABLED` | `false` | When true, expose Prometheus `/metrics` (no `X-Api-Key`; firewall the port it lands on). `SANDBOX_API_METRICS_LISTEN_ADDR` chooses the listener. See [Metrics](#metrics). |
| `SANDBOX_API_RUNNER_HEARTBEAT_GRACE` | `45s` | How long after the last gRPC heartbeat a runner remains eligible for placement (Go [`time.ParseDuration`](https://pkg.go.dev/time#ParseDuration) syntax, e.g. `45s`, `2m`) |
| `SANDBOX_API_IDLE_STOP_AFTER` | `1h` | Idle time after which the sweeper stops a sandbox (deletes it if `ephemeral`). `0` disables |
| `SANDBOX_API_IDLE_DELETE_AFTER` | `24h` | Idle time after which the sweeper deletes a sandbox; wakes are refused past this window. `0` disables |
| `SANDBOX_API_IDLE_DELETE_SAFETY_BUFFER` | `1m` | Added to a sandbox's idle window before deletion as a race guard (applied when either window above is > 0) |
| `SANDBOX_API_IDLE_SWEEP_INTERVAL` | `1m` | How often the idle sweeper runs |
| `SANDBOX_API_IDLE_SWEEP_CONCURRENCY` | `8` | Runner stop/delete calls one sweep keeps in flight (1–256), spread across runners. With Postgres the sandbox-lock pool is this plus 5 |
| `SANDBOX_API_ORPHAN_REAP_BUFFER` | `5m` | How long after a runner deregisters before the idle sweeper removes its orphaned sandbox rows from the store |
| `SANDBOX_API_GRPC_TLS_CERT_FILE` | *(required)* | Server certificate (PEM) for the registration gRPC listener |
| `SANDBOX_API_GRPC_TLS_KEY_FILE` | *(required)* | Server private key (PEM) |
| `SANDBOX_API_GRPC_TLS_CLIENT_CA_FILE` | *(required)* | CA bundle (PEM) that signed runner client certificates |
| `SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CA_FILE` | *(required)* | CA (PEM) that signed runner **SandboxControl** and HTTP server certs |
| `SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CERT_FILE` | *(required)* | API client certificate (PEM) for SandboxControl and the runner HTTP proxy |
| `SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_KEY_FILE` | *(required)* | API client key (PEM) |
| `SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_SERVER_NAME` | *(empty)* | TLS verify name for the **SandboxControl gRPC channel only**, when it must differ from the dial host (defaults to the runner host). The HTTP proxy always verifies each runner against the host in its own advertised base URL, so this never applies there |

**Heartbeat grace:** Runners stay registered while their gRPC stream is open and heartbeats are written to the store (Postgres) or in-memory registry (SQLite). Between heartbeats, the API still considers a runner usable for new placements only if its last heartbeat was within `SANDBOX_API_RUNNER_HEARTBEAT_GRACE`. After that window, the runner is skipped until the next heartbeat.

**Multi-pod (Postgres):** Set `SANDBOX_API_STORE=postgres` and the `SANDBOX_API_POSTGRES_*` variables when running multiple API replicas. Sandbox metadata and runner heartbeats are shared in Postgres; the idle sweeper uses a Postgres advisory lock so only one pod sweeps at a time. New sandboxes are placed on the eligible runner with the lowest reported `capacity_used`. Disable `api.persistence` in Helm when using Postgres (state lives in the database, not local disk).

The idle sweeper waits `SANDBOX_API_ORPHAN_REAP_BUFFER` (default `5m`) after a runner's last heartbeat before removing its orphaned sandbox rows from the API store (sandboxes that are already idle stop/delete candidates). With SQLite, this is based on observing the runner stream close; with Postgres, it is based on `last_seen` in the shared registry.

## Runner

### Shared runner config

| Variable | Default | Description |
| --- | --- | --- |
| `SANDBOX_RUNNER_API_KEYS` | *(required)* | Comma-separated list of valid internal API keys accepted from the API container |
| `SANDBOX_RUNNER_LOG_LEVEL` | `info` | Minimum log severity (`debug`, `info`, `warn`, `error`; case-insensitive) |
| `SANDBOX_RUNNER_LISTEN_ADDR` | `:8080` | HTTPS listen address; serves TLS with the `SANDBOX_RUNNER_CONTROL_GRPC_TLS_*` material |
| `SANDBOX_RUNNER_API_GRPC_ADDR` | *(required)* | API `host:port` for gRPC registration |
| `SANDBOX_RUNNER_REGISTRATION_TOKEN` | *(required)* | Must match `SANDBOX_API_RUNNER_REGISTRATION_TOKEN` on the API |
| `SANDBOX_RUNNER_HTTP_BASE_URL` | *(required)* | Base URL the API uses to reach this runner; its host must match a SAN on the runner's control cert (e.g. `https://runner:8080`). An `http://` value is upgraded to `https://` with a warning, since the listener only serves TLS; the API rejects any registration whose base URL is not `https://` |
| `SANDBOX_RUNNER_ID` | hostname | Stable runner id sent to the API |
| `SANDBOX_RUNNER_CAPACITY_TOTAL` | `1000` | Reported capacity for placement (`0` = unlimited) |
| `SANDBOX_RUNNER_DATA_DIR` | `/var/sandboxes` | Per-sandbox data directory (the Firecracker runner keeps each sandbox's rootfs here) |
| `SANDBOX_RUNNER_MAX_FILE_BYTES` | `10485760` | Maximum body the runner accepts on file write/append (10 MB); the API applies `SANDBOX_API_MAX_FILE_BYTES` first |
| `SANDBOX_RUNNER_METRICS_ENABLED` | `false` | When true, expose Prometheus `/metrics` on the runner's HTTP listener (no `X-Api-Key` and no client certificate; firewall the port). See [Metrics](#metrics). |
| `SANDBOX_RUNNER_REGISTRATION_GRPC_CA_FILE` | *(required)* | CA (PEM) that signed the API registration gRPC server cert |
| `SANDBOX_RUNNER_REGISTRATION_GRPC_CERT_FILE` | *(required)* | Runner client cert (PEM) for registration mTLS |
| `SANDBOX_RUNNER_REGISTRATION_GRPC_KEY_FILE` | *(required)* | Runner client key (PEM) for registration mTLS |
| `SANDBOX_RUNNER_REGISTRATION_GRPC_SERVER_NAME` | *(empty)* | TLS name to verify on the API registration cert (defaults to host from `SANDBOX_RUNNER_API_GRPC_ADDR`) |
| `SANDBOX_RUNNER_CONTROL_GRPC_LISTEN_ADDR` | `:9091` | Listen address for **SandboxControl** gRPC |
| `SANDBOX_RUNNER_CONTROL_GRPC_ADVERTISE_ADDR` | *(derived)* | `host:port` sent to the API in heartbeats; required if listen is set and `SANDBOX_RUNNER_HTTP_BASE_URL` cannot be used to derive host/port |
| `SANDBOX_RUNNER_CONTROL_GRPC_TLS_CERT_FILE` | *(required)* | Server cert (PEM) for SandboxControl and the HTTP listener |
| `SANDBOX_RUNNER_CONTROL_GRPC_TLS_KEY_FILE` | *(required)* | Server private key (PEM) |
| `SANDBOX_RUNNER_CONTROL_GRPC_TLS_CLIENT_CA_FILE` | *(required)* | CA (PEM) that signed API client certificates, for SandboxControl and the HTTP listener |

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
| `SANDBOX_RUNNER_DOCKER_STORAGE_DRIVER` | *(dockerd default)* | Storage driver for the inner dockerd when disk quotas are off (e.g. `vfs` where nested overlayfs is unavailable). Ignored with quotas on, which always use `overlay2`. Read by `scripts/start-runner.sh`. |

### Firecracker runner backend config

These variables are parsed by the Firecracker runner entrypoint.

#### Firecracker slots

A Firecracker slot is one schedulable VM position on a runner. The runner creates slots from `SANDBOX_RUNNER_CAPACITY_TOTAL`; if capacity is `100`, the runner has slots `0` through `99`. A sandbox occupies one slot from create until stop/delete.

Slots give the host-side Firecracker resources stable names without exposing those details to the API or clients. For slot `n`, the runtime derives:

- Network namespace: `fc-sb-n`
- TAP name inside the namespace: `SANDBOX_RUNNER_FIRECRACKER_HOST_TAP_DEVICE_NAME`
- Host veth `fc-veth-n` with a `/30` uplink carved from `10.200.0.0/16`, which caps capacity at 16384
- Host-local daemon proxy port: `SANDBOX_RUNNER_FIRECRACKER_PROXY_PORT_START + n`

| Variable | Default | Description |
| --- | --- | --- |
| `SANDBOX_RUNNER_FIRECRACKER_JAILER_BIN` | `/opt/firecracker/bin/jailer` | Path to the Firecracker jailer binary |
| `SANDBOX_RUNNER_FIRECRACKER_BIN` | `/opt/firecracker/bin/firecracker` | Path to the Firecracker VMM binary |
| `SANDBOX_RUNNER_FIRECRACKER_JAILER_BASE_DIR` | `/srv/jailer` | Base directory passed to `jailer --chroot-base-dir` |
| `SANDBOX_RUNNER_FIRECRACKER_TEMPLATE_DIR` | `/srv/firecracker/template` | Directory containing the snapshot rootfs (`rootfs.ext4`) |
| `SANDBOX_RUNNER_FIRECRACKER_SNAPSHOT_MEM_PATH` | `/srv/firecracker/snapshots/mem` | Host path bind-mounted into the jail as `/snapshot_mem` |
| `SANDBOX_RUNNER_FIRECRACKER_SNAPSHOT_STATE_PATH` | `/srv/firecracker/snapshots/state` | Host path bind-mounted into the jail as `/snapshot_state`. When auto-create is enabled, must share a directory with `SNAPSHOT_MEM_PATH` |
| `SANDBOX_RUNNER_FIRECRACKER_GUEST_IP` | `172.16.0.10` | Guest IP expected by the restored snapshot |
| `SANDBOX_RUNNER_FIRECRACKER_HOST_TAP_DEVICE_NAME` | `fc-tap-0` | TAP device name inside each sandbox netns |
| `SANDBOX_RUNNER_FIRECRACKER_HOST_TAP_IP_CIDR` | `172.16.0.1/24` | Host-side TAP address inside each sandbox netns |
| `SANDBOX_RUNNER_FIRECRACKER_DAEMON_PORT` | `8081` | Sandbox daemon port inside the guest |
| `SANDBOX_RUNNER_FIRECRACKER_PROXY_LISTEN_IP` | `127.0.0.1` | Host-side listen IP for daemon proxies |
| `SANDBOX_RUNNER_FIRECRACKER_PROXY_PORT_START` | `18081` | First host-side proxy port. Slot `n` uses `PROXY_PORT_START+n`. |
| `SANDBOX_RUNNER_FIRECRACKER_SOCKET_WAIT_ATTEMPTS` | `120` | Number of checks while waiting for `firecracker.socket` |
| `SANDBOX_RUNNER_FIRECRACKER_SOCKET_WAIT_INTERVAL_MS` | `20` | Delay between Firecracker socket checks in milliseconds |
| `SANDBOX_RUNNER_FIRECRACKER_DAEMON_WAIT_TIMEOUT` | `60s` | Maximum time to wait for guest daemon health after snapshot restore |
| `SANDBOX_RUNNER_FIRECRACKER_MANIFEST_PATH` | _(empty)_ | Optional absolute path to release `MANIFEST.json` for git_sha / daemon checksum pinning |
| `SANDBOX_RUNNER_FIRECRACKER_EXPECTED_GIT_SHA` | _(empty)_ | When set, must match `git_sha` in the manifest (requires `MANIFEST_PATH`) |
| `SANDBOX_RUNNER_FIRECRACKER_CREATE_SNAPSHOT_SCRIPT` | _(empty)_ | Absolute path to `create-golden-snapshot.sh`. When set and any of `snapshot_mem`, `snapshot_state` or `boot.json` is missing, Prepare runs it (mem/state paths must share a directory). Production Firecracker hosts set this so the runner owns host-local snapshot creation. Empty = do not auto-create |
| `SANDBOX_RUNNER_FIRECRACKER_DAEMON_BIN` | `/srv/firecracker/bin/sandbox-daemon` | Host path to `sandbox-daemon` used for golden snapshot create and optional manifest checksum |

The runner reports healthy only after `Prepare` has verified these assets, created the snapshot if needed, and passed an admission canary — see [the runtime README](../internal/runner/runtime/firecracker.ee/README.md#admission-prepare--ready).

#### Golden snapshot boot parameters (`boot.json`)

`create-golden-snapshot.sh` writes `boot.json` next to `snapshot_mem` and `snapshot_state`, recording what it sent to the Firecracker API: `vcpu_count`, `mem_size_mib`, `kernel_image_path`, `boot_args`, `rootfs_drive_path`, `guest_mac`, `guest_ip`, `host_tap_device_name`, `daemon_port`. Paths are as Firecracker sees them inside the jail. The runner has no configuration for most of these; it replays them when it cold boots a sandbox after a guest crash, so recovery stays faithful to how the snapshot was built.

- `kernel_image_path` and `rootfs_drive_path` are where the runner bind-mounts those assets into each jail (absolute, no `.`/`..` segments). Because the kernel path names a mount target, the runner also records the size and mtime of `TemplateDir/vmlinux` at create and refuses a later cold boot if they changed — roll a new kernel out by replacing the runner, not by rebuilding the template underneath it.
- Admission fails if `boot.json` contradicts the runner on `guest_ip`, `host_tap_device_name`, `daemon_port` or the gateway in `boot_args` (third field of the kernel `ip=` parameter; must equal the host address in `SANDBOX_RUNNER_FIRECRACKER_HOST_TAP_IP_CIDR`). These are baked into the guest, so a mismatch would otherwise yield sandboxes that never answer — except the gateway, where the guest still answers the proxy (and passes the canary) but has no egress.
- Auto-create passes the runner's `GUEST_IP`, `HOST_TAP_IP_CIDR`, `HOST_TAP_DEVICE_NAME` and `DAEMON_PORT` to the script. When running it by hand, set the same variables. `MEM_MIB` and `VCPUS` are script-owned.
- With auto-create enabled, `SNAPSHOT_MEM_PATH`/`SNAPSHOT_STATE_PATH` must be the generated files or symlinks to them; an independent copy would keep serving an old snapshot under a `boot.json` describing the new one, and admission rejects that.
- The three files describe one build. A missing `boot.json` is an incomplete snapshot: with the create script configured, `Prepare` rebuilds the set; otherwise admission fails naming the script and `--out` directory to re-run.

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

The API and runner can each expose a Prometheus `/metrics` endpoint. Set `SANDBOX_API_METRICS_ENABLED=true` and/or `SANDBOX_RUNNER_METRICS_ENABLED=true` to enable. By default both serve it on the same HTTP port that serves their public API; the API can move it to a port of its own. The endpoint:

- Bypasses `X-Api-Key`, matching the n8n core operator model. Operators are expected to firewall the port it lands on or front it with a private LB; otherwise anyone reaching the listener can read the metrics.
- Uses the `sandbox_` namespace, with a `role` label (`api` or `runner`) on every metric so series from both binaries can live in one Prometheus.
- Bounds cardinality by labeling HTTP series with the route pattern (e.g. `/sandboxes/{id}/executions`), not the raw path.

### A dedicated metrics port for the API

`SANDBOX_API_METRICS_LISTEN_ADDR` moves the API's `/metrics` to its own listener, so the scrape port can be restricted independently of the port an ingress publishes:

```sh
SANDBOX_API_LISTEN_ADDR=:8080
SANDBOX_API_METRICS_ENABLED=true
SANDBOX_API_METRICS_LISTEN_ADDR=127.0.0.1:9100
```

- Leaving it empty, or giving it the same port as `SANDBOX_API_LISTEN_ADDR`, keeps `/metrics` on the API listener. That is the default and matches earlier releases.
- A different port starts a second HTTP server that serves `GET /metrics` and nothing else: no auth, no access log, no CORS, and 404 for every other path. `/metrics` is then a 404 on the public port, and `/healthz` stays there.
- Using the same port with a different host, or the `SANDBOX_API_GRPC_LISTEN_ADDR` port, is rejected at startup. So is a port already in use, which fails the process rather than starting half-configured.
- A dedicated listener does not record its own scrapes, so no `route="/metrics"` series appear. On the shared listener they still do. Sums over `sandbox_http_requests_total` therefore step down when a deployment splits the port.

The runner always serves `/metrics` on its own HTTPS listener; it has no equivalent setting.

Series exposed today:

- `sandbox_http_requests_total{role,route,method,status}` and `sandbox_http_request_duration_seconds{role,route,method}` (both binaries).
- API: `sandbox_sandbox_operations_total{operation,result}`, `sandbox_sandboxes_active`, `sandbox_runners_registered`.
- Runner: `sandbox_container_operations_total{operation,result}`, `sandbox_container_operation_duration_seconds{operation}`, `sandbox_containers_active`, plus `sandbox_guest_deaths_total`, sandbox guests that died on their own, and `sandbox_recoveries_total{result}`, attempts to bring one back.
- Firecracker runner: `sandbox_lifecycle_step_duration_seconds{operation,step}`, the per-step breakdown of a create, wake or recovery, and `sandbox_slots_unwired`, slots whose network namespace is not yet built (zero in steady state). See [observability.md](observability.md).
- Plus the standard `go_*` and `process_*` collectors.

## Disk quotas

When `SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB > 0`, the runner emits `--storage-opt size=Nm` on each sandbox so the inner dockerd caps that sandbox's writable layer. To make the flag enforce anything, `scripts/start-runner.sh` allocates a loopback xfs image (sized from `SANDBOX_RUNNER_DISK_QUOTA_POOL_SIZE_GB` — see the table above for how the default is derived), formats it, mounts it with `prjquota` at `/var/lib/docker`, and starts the inner dockerd with `--storage-driver=overlay2` against that mount. When `SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB` is unset/`0`, the pool is not created and dockerd uses its default storage with no per-sandbox enforcement. `start-runner.sh` reports a successful mount to the runner by exporting `SANDBOX_RUNNER_DISK_QUOTA_ACTIVE=true`; it is not an operator setting.

The image itself is not bounded by the mount it backs. Put it on a volume with a size limit, and keep `SANDBOX_RUNNER_DISK_QUOTA_POOL_SIZE_GB` within that limit. `SANDBOX_RUNNER_DISK_QUOTA_POOL_PATH` moves the image. The Helm chart does this for you; see the chart README section 'Disk Quotas'.

**Host kernel requirement:** the runner container's host kernel must be built with `CONFIG_XFS_QUOTA` (=y or =m). Every mainstream Linux distro kernel ships with this enabled. The notable exception is **Docker Desktop's linuxkit kernel** on macOS, which omits it — on that host the loopback mount fails and the runner logs `disk quota enforcement: DISABLED` and continues without per-sandbox enforcement. Sandboxes still run, just without writable-layer caps. To check a node: `zcat /proc/config.gz | grep XFS_QUOTA` or `cat /boot/config-$(uname -r) | grep XFS_QUOTA`.
