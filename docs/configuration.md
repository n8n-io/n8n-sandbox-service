# Configuration

All services are configured via environment variables. In the variables the API, runner, daemon and code daemon binaries read, surrounding whitespace is ignored (except in `SANDBOX_API_POSTGRES_PASSWORD`), an empty value counts as unset, and a value that does not parse stops the service at startup. This does not cover the variables `scripts/start-runner.sh` reads. [limits.md](limits.md) lists the limits clients and operators run into.

## Contents

- [API](#api)
- [Runner](#runner)
- [Sandbox daemon](#sandbox-daemon)
- [Code daemon](#code-daemon)
- [Metrics](#metrics)
- [Disk quotas](#disk-quotas)

## API

| Variable | Default | Description |
| --- | --- | --- |
| `SANDBOX_API_KEYS` | *(required)* | Comma-separated admin API keys. Full access to all sandboxes and `/admin/tenants` key management. Self-hosted can use these alone without minting tenant keys. |
| `SANDBOX_API_PROVISIONER_KEYS` | *(empty)* | Comma-separated provisioner API keys ([security-model.md](security-model.md#provisioner-keys)). When set, startup fails if a key is also in `SANDBOX_API_KEYS` or if `SANDBOX_API_DEFAULT_MAX_SANDBOXES` is `0`. |
| `SANDBOX_API_SCALER_KEYS` | *(empty)* | Comma-separated scaler API keys: `GET /admin/runners` and `PUT`/`DELETE /admin/runners/{id}/cordon` only ([security-model.md](security-model.md#scaler-keys)). Startup fails if a key is also in `SANDBOX_API_KEYS` or `SANDBOX_API_PROVISIONER_KEYS`. |
| `SANDBOX_API_VOLUME_PUBLISHER_KEYS` | *(empty)* | Comma-separated volume publisher API keys: the `/shared-volumes` routes only ([security-model.md](security-model.md#volume-publisher-keys)). Startup fails if a key is also in `SANDBOX_API_KEYS`, `SANDBOX_API_PROVISIONER_KEYS` or `SANDBOX_API_SCALER_KEYS`. Without `SANDBOX_API_VOLUME_STORE` the routes answer these keys `501`, and startup logs a warning. |
| `SANDBOX_API_RUNNER_REGISTRATION_TOKEN` | *(required)* | Shared secret; runners authenticate to the private gRPC registration service with `Authorization: Bearer …` |
| `SANDBOX_API_RUNNER_API_KEY` | *(empty)* | Optional API key injected by the API when calling runner HTTP; the runner also requires the client certificate above |
| `SANDBOX_API_LOG_LEVEL` | `info` | Minimum log severity (`debug`, `info`, `warn`, `error`; case-insensitive) |
| `SANDBOX_API_LISTEN_ADDR` | `:8080` | Public HTTP listen address |
| `SANDBOX_API_GRPC_LISTEN_ADDR` | `:9090` | Private gRPC listen address for runner registration streams |
| `SANDBOX_API_METRICS_LISTEN_ADDR` | *(empty)* | Dedicated `host:port` for Prometheus `/metrics`. Empty, or the same port as `SANDBOX_API_LISTEN_ADDR`, serves `/metrics` on the public listener; any other port starts a second server that serves only `/metrics`. Must not use the `SANDBOX_API_GRPC_LISTEN_ADDR` port. Ignored, with a warning, when `SANDBOX_API_METRICS_ENABLED=false`. See [Metrics](#metrics). |
| `SANDBOX_API_STORE` | `sqlite` | Store backend: `sqlite` (default, single API pod) or `postgres` (multi-pod) |
| `SANDBOX_API_DATA_DIR` | `/var/lib/n8n-sandbox-api` | SQLite store directory when `SANDBOX_API_STORE=sqlite`, and where `SANDBOX_API_VOLUME_LOCAL_DIR` lies by default; must exist and be writable. Mount a persistent volume here to keep the SQLite store, and the local volume store in its default place, across API pod restarts. With `SANDBOX_API_SQLITE_WAL=true`, use a local or block volume, not a network file system. |
| `SANDBOX_API_SQLITE_WAL` | `true` | Open the SQLite store in WAL mode with `synchronous=NORMAL`. `false` switches to the DELETE rollback journal with `synchronous=FULL`, including databases previously opened in WAL mode. The API fails to start if SQLite does not apply the requested journal mode. |
| `SANDBOX_API_POSTGRES_HOST` | *(required with postgres)* | Postgres host |
| `SANDBOX_API_POSTGRES_PORT` | `5432` | Postgres port |
| `SANDBOX_API_POSTGRES_USER` | *(required with postgres)* | Postgres user |
| `SANDBOX_API_POSTGRES_PASSWORD` | *(required with postgres)* | Postgres password |
| `SANDBOX_API_POSTGRES_DB` | *(required with postgres)* | Postgres database name |
| `SANDBOX_API_POSTGRES_SSLMODE` | `require` | Postgres TLS mode (`disable`, `require`, `verify-full`, etc.) |
| `SANDBOX_API_VOLUME_STORE` | *(empty)* | Where volume objects, such as [shared volumes](API.md#shared-volumes), are stored: empty (no store, and no shared volumes), `local` or `remote`. `local` keeps them in `SANDBOX_API_VOLUME_LOCAL_DIR` and serves them to runners on a listener of its own (`SANDBOX_API_VOLUME_LOCAL_LISTEN_ADDR`), under signed URLs. Without `SANDBOX_API_VOLUME_LOCAL_SIGNING_KEY` it signs with a key that is random per process, so like `sqlite` it needs a single API instance, and its URLs stop working on restart; several instances can [share it](#sharing-the-local-volume-store). `remote` hands them to a [store helper](volume-store-helper.md) next to the API, which holds the storage credentials and issues the read URLs |
| `SANDBOX_API_VOLUME_LOCAL_LISTEN_ADDR` | `:8443` | With `local`, the `host:port` of the listener that serves `GET /volume-objects/…` and nothing else. It speaks TLS 1.3 and requires a client certificate, with the registration listener's `SANDBOX_API_GRPC_TLS_*` files, so it accepts the certificates runners register with ([security-model.md](security-model.md#api-to-runner)). Its port must differ from those of `SANDBOX_API_LISTEN_ADDR`, `SANDBOX_API_GRPC_LISTEN_ADDR` and `SANDBOX_API_METRICS_LISTEN_ADDR`. Not read without `local` |
| `SANDBOX_API_VOLUME_LOCAL_URL` | *(required with local)* | Base URL at which runners reach that listener, such as `https://sandbox-api:8443`: `https` with a host and optional port, and no credentials, path, query or fragment (a trailing `/` is ignored). The host must be the one runners register with (the host of their `SANDBOX_RUNNER_API_GRPC_ADDR`). The listener presents the registration certificate (`SANDBOX_API_GRPC_TLS_CERT_FILE`), which must carry that host, or the runners' `SANDBOX_RUNNER_REGISTRATION_GRPC_SERVER_NAME` if set. Read URLs point at `<url>/volume-objects/…` |
| `SANDBOX_API_VOLUME_LOCAL_DIR` | `<SANDBOX_API_DATA_DIR>/volumes` | With `local`, the absolute path of the store's directory. The API creates it if its parent exists, sets its mode to `0700`, and refuses to start unless its file system makes hard links. At startup it removes upload temp files in it that are older than a day. Not read without `local` |
| `SANDBOX_API_VOLUME_LOCAL_SIGNING_KEY` | *(empty)* | With `local`, the key that signs read URLs, at least 32 bytes in standard base64, padded or not (`openssl rand -base64 32`). Every API instance with the key accepts the URLs any of them signs, and URLs outlive restarts. Unset means a key random per process; set but empty is refused. Never logged ([security-model.md](security-model.md#api-to-runner)). Not read without `local` |
| `SANDBOX_API_VOLUME_LOCAL_PREVIOUS_SIGNING_KEY` | *(empty)* | With `local`, a key in the same form that verifies read URLs but signs none, for a rotation. Needs `SANDBOX_API_VOLUME_LOCAL_SIGNING_KEY`. Not read without `local` |
| `SANDBOX_API_VOLUME_REMOTE_SOCKET` | *(required with remote)* | Absolute path, at most 107 bytes, of the Unix socket the store helper serves gRPC on. The helper authenticates nobody, so only the API and the helper may reach it |
| `SANDBOX_API_VOLUME_BUILD` | *(empty)* | What the service's runners attach: tars when empty (Docker), ext4 images with `firecracker`, which a runner builds at each [shared volume](API.md#shared-volumes) publish. Must match the runners. `firecracker` needs `SANDBOX_API_VOLUME_STORE`. Volumes published before it was set have no image |
| `SANDBOX_API_MAX_FILE_BYTES` | `10485760` | Maximum file upload size (10 MB), also the compressed size of an archive upload |
| `SANDBOX_API_MAX_TASK_INPUT_BYTES` | `67108864` | Total size of the input files of a code sandbox task (64 MiB; up to 1 GiB). The API refuses a task request body more than 1 MiB larger, the room for the `spec` and the multipart headers. The runner and the daemon (`CODE_DAEMON_MAX_INPUT_BYTES`) apply their own limits |
| `SANDBOX_API_DEFAULT_MAX_SANDBOXES` | `50` | Default per-tenant sandbox quota when `POST /admin/tenants` omits `max_sandboxes` (`0` = unlimited), and the highest `max_sandboxes` a provisioner key may set on create or `PATCH`. Must fit Postgres/SQLite `INTEGER` (`0`…`2147483647`). |
| `SANDBOX_API_MAX_INFLIGHT_PER_TENANT` | `512` | Requests a tenant may have in progress at once on this replica, all its keys together; more get `429` ([what counts](API.md#http-429--too-many-requests-in-progress)). A first estimate: 32 sandboxes with 16 requests in progress each. It also bounds what a tenant holds here while requests wait on a slow runner, which the per-sandbox limit cannot see. `0` = no limit, for execution `DELETE`s too |
| `SANDBOX_API_KEY_CACHE_TTL` | `15s` | How long each replica remembers a tenant key lookup (`0`–`1m`, `0` = no cache; [how](architecture.md#tenant-key-cache)). A revoked key, or the key of a deleted tenant, still works on other replicas for up to this long |
| `SANDBOX_API_ENABLE_CORS` | `false` | Enable CORS headers (allow all origins); needed for the browser playground |
| `SANDBOX_API_CODE_SANDBOXES_ENABLED` | `false` | Serve the [code sandbox](API.md#code-sandboxes) routes. When `false`, they answer `404` |
| `SANDBOX_API_METRICS_ENABLED` | `false` | When true, expose Prometheus `/metrics` (no `X-Api-Key`; firewall the port it lands on). `SANDBOX_API_METRICS_LISTEN_ADDR` chooses the listener. See [Metrics](#metrics). |
| `SANDBOX_API_RUNNER_HEARTBEAT_GRACE` | `45s` | How long after the last gRPC heartbeat a runner remains eligible for placement (Go [`time.ParseDuration`](https://pkg.go.dev/time#ParseDuration) syntax, e.g. `45s`, `2m`) |
| `SANDBOX_API_IDLE_STOP_AFTER` | `1h` | Idle time after which the sweeper stops a sandbox (deletes it if `ephemeral`). `0` disables. Extended by twice `SANDBOX_API_ACTIVITY_WRITE_INTERVAL`, and by four times that before an ephemeral sandbox is deleted ([why](architecture.md#sandbox-activity)) |
| `SANDBOX_API_IDLE_DELETE_AFTER` | `24h` | Idle time after which the sweeper deletes a sandbox; wakes are refused past this window. `0` disables. Wakes are refused twice `SANDBOX_API_ACTIVITY_WRITE_INTERVAL` later, deletes come four times that later ([why](architecture.md#sandbox-activity)) |
| `SANDBOX_API_ACTIVITY_WRITE_INTERVAL` | `5s` | How often each replica writes the `last_active_at` of the sandboxes it served, in one statement (`1s`–`1m`; [how](architecture.md#sandbox-activity)). Longer means fewer database writes, and idle stops that act up to twice that later, deletes four times |
| `SANDBOX_API_MAX_RESPONSE_ACTIVITY` | `30m` | How long a response that is still open, such as an exec stream, keeps its sandbox from going idle (at least `1m`). Keep it at least `SANDBOX_RUNNER_MAX_EXEC_TIMEOUT` |
| `SANDBOX_API_IDLE_DELETE_SAFETY_BUFFER` | `1m` | Added to a sandbox's idle window before deletion as a race guard (applied when either window above is > 0) |
| `SANDBOX_API_IDLE_SWEEP_INTERVAL` | `1m` | How often the idle sweeper runs |
| `SANDBOX_API_IDLE_SWEEP_CONCURRENCY` | `8` | Runner stop/delete calls one sweep keeps in flight (1–16), spread across runners. With Postgres the sandbox-lock pool is this plus 5 |
| `SANDBOX_API_ORPHAN_REAP_BUFFER` | `5m` | How long after a runner deregisters before the idle sweeper removes its orphaned sandbox rows from the store |
| `SANDBOX_API_SCALER_URL` | *(empty)* | Base URL of the runner scaler, such as `http://scaler.internal:8090`: scheme, host and port only, no path or query (a trailing `/` is ignored). `GET /admin/scaler` forwards to `<url>/policy`. Empty disables the endpoint, which then answers `503`. |
| `SANDBOX_API_GRPC_TLS_CERT_FILE` | *(required)* | Server certificate (PEM) for the registration gRPC listener, and for the volume-objects listener with `SANDBOX_API_VOLUME_STORE=local` |
| `SANDBOX_API_GRPC_TLS_KEY_FILE` | *(required)* | Server private key (PEM) |
| `SANDBOX_API_GRPC_TLS_CLIENT_CA_FILE` | *(required)* | CA bundle (PEM) that signed runner client certificates; both listeners accept only those |
| `SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CA_FILE` | *(required)* | CA (PEM) that signed runner **SandboxControl** and HTTP server certs |
| `SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CERT_FILE` | *(required)* | API client certificate (PEM) for SandboxControl and the runner HTTP proxy |
| `SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_KEY_FILE` | *(required)* | API client key (PEM) |
| `SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_SERVER_NAME` | *(empty)* | TLS verify name for the **SandboxControl gRPC channel only**, when it must differ from the dial host (defaults to the runner host). The HTTP proxy always verifies each runner against the host in its own advertised base URL, so this never applies there |

**Heartbeat grace:** Runners stay registered while their gRPC stream is open and heartbeats are written to the store (Postgres) or in-memory registry (SQLite). Between heartbeats, the API still considers a runner usable for new placements only if its last heartbeat was within `SANDBOX_API_RUNNER_HEARTBEAT_GRACE`. After that window, the runner is skipped until the next heartbeat.

**Multi-pod (Postgres):** Set `SANDBOX_API_STORE=postgres` and the `SANDBOX_API_POSTGRES_*` variables when running multiple API replicas. Sandbox metadata and runner heartbeats are shared in Postgres; the idle sweeper uses a Postgres advisory lock so only one pod sweeps at a time. New sandboxes are placed on the eligible runner with the lowest reported `capacity_used`. Disable `api.persistence` in Helm when using Postgres (state lives in the database, not local disk). Each replica opens up to 25 store connections plus `SANDBOX_API_IDLE_SWEEP_CONCURRENCY + 5` lock connections; size Postgres `max_connections` for every replica and other clients.

The idle sweeper waits `SANDBOX_API_ORPHAN_REAP_BUFFER` (default `5m`) after a runner's last heartbeat before removing its orphaned sandbox rows from the API store (sandboxes that are already idle stop/delete candidates). With SQLite, this is based on observing the runner stream close; with Postgres, it is based on `last_seen` in the shared registry.

### Sharing the local volume store

Several API instances, such as API pods on Postgres, can share `SANDBOX_API_VOLUME_STORE=local` with:

- the same `SANDBOX_API_VOLUME_LOCAL_SIGNING_KEY` in every instance;
- `SANDBOX_API_VOLUME_LOCAL_DIR` on one volume that all of them mount read-write (`ReadWriteMany`), on a file system with hard links, such as NFS (Amazon EFS, Google Filestore, Azure Files over NFS, Longhorn) or CephFS. SMB shares, such as Azure Files over SMB, have no hard links;
- NFS mounted with `lookupcache=positive`. By default an NFS client remembers names it found missing, so an object another instance commits can look missing for up to a minute. The API warns at startup when the directory is on NFS without that option;
- NFS exported `sync`, the default, and every instance running as the same user.

Every instance serves downloads: `SANDBOX_API_VOLUME_LOCAL_URL` names all of them, such as a Kubernetes Service does, so one instance can serve a URL another signed. The per-scope download cap (`SANDBOX_API_MAX_INFLIGHT_PER_TENANT`) applies per instance. On NFS, deleting an object also ends the downloads of it in progress on other instances.

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
| `SANDBOX_RUNNER_CAPACITY_TOTAL` | Docker `1000`; Firecracker derived | Reported capacity for placement (`0` = unlimited on Docker; Firecracker needs a positive value). Docker does not derive it: size it from the host's memory and each sandbox's memory limit. On Firecracker a set value may only lower the derived count ([slots](#firecracker-slots)) |
| `SANDBOX_RUNNER_DATA_DIR` | `/var/sandboxes` | Per-sandbox data directory (Firecracker: see [Data volume](../internal/runner/runtime/firecracker.ee/README.md#data-volume)) |
| `SANDBOX_RUNNER_VOLUME_CACHE_DIR` | `<data dir>/volume-cache` | Where the runner caches downloaded volume objects. Wiped at startup. Must be an absolute path, and not `/`, the data dir or a directory that contains it. The runner creates it, but not its parent |
| `SANDBOX_RUNNER_VOLUME_CACHE_MAX_BYTES` | `21474836480` | Volume cache budget (20 GiB): allocated bytes of cached objects plus the room fetches in progress set aside: an object's size, and for a `tar` also the most its unpacked files can take, since the tar stays on disk until they are complete. An evicted object counts until its files are removed. On the data volume, Firecracker creates and volume builds count the budget less the objects it has finished downloading as taken ([data volume](../internal/runner/runtime/firecracker.ee/README.md#data-volume)). A download starts only if it fits the budget and leaves 5% of the disk, and of its inodes, free, evicting unused objects least recently used first. If objects in use and downloads in progress still leave no room, or the disk fills during the download, it is refused like a full runner; an object larger than the whole budget fails outright. 20 GiB holds several multi-GB images and leaves most of a data volume of a few hundred GiB to sandboxes; lower it on small disks |
| `SANDBOX_RUNNER_VOLUME_FETCH_TIMEOUT` | `5m` | Longest a volume object download may take, with a `tar`'s unpack after it ([Docker runtime](../internal/runner/runtime/docker/README.md#volumes)). Downloads are `https` only and follow no redirects. A URL on the host of `SANDBOX_RUNNER_API_GRPC_ADDR`, on any port, is fetched with the `SANDBOX_RUNNER_REGISTRATION_GRPC_*` certificate and CA and without a proxy, even where the registration stream goes through `HTTPS_PROXY`; any other URL with the system's CAs, the proxy from the environment and no client certificate |
| `SANDBOX_RUNNER_MAX_FILE_BYTES` | `10485760` | Maximum body the runner accepts on file write/append and archive upload (10 MB); the API applies `SANDBOX_API_MAX_FILE_BYTES` first |
| `SANDBOX_RUNNER_MAX_TASK_INPUT_BYTES` | `67108864` | Total size of the input files of a code sandbox task (64 MiB; up to 1 GiB). The runner refuses a task request body more than 1 MiB larger. The API applies `SANDBOX_API_MAX_TASK_INPUT_BYTES` first, and the daemon `CODE_DAEMON_MAX_INPUT_BYTES` after |
| `SANDBOX_RUNNER_MAX_INFLIGHT_PER_SANDBOX` | `256` | Requests one sandbox may have in progress at once; more get `429` ([what counts](API.md#http-429--too-many-requests-in-progress)). Room for clients that write many files in parallel, while still bounding what one sandbox holds on its runner. `0` = no limit, for execution `DELETE`s too |
| `SANDBOX_RUNNER_MAX_EXEC_TIMEOUT` | `15m` | Largest `timeout_ms` an exec may ask for; a larger one gets `400`. A first estimate: long enough for builds and test suites, and what ends a forgotten command, which keeps running after its stream closes. `0` (no limit) or at least `5m`, the timeout a command gets when it sets none. Keep the API's `SANDBOX_API_MAX_RESPONSE_ACTIVITY` at least this long |
| `SANDBOX_RUNNER_METRICS_ENABLED` | `false` | When true, expose Prometheus `/metrics` on the runner's HTTP listener (no `X-Api-Key` and no client certificate; firewall the port). See [Metrics](#metrics). |
| `SANDBOX_RUNNER_REGISTRATION_GRPC_CA_FILE` | *(required)* | CA (PEM) that signed the API registration gRPC server cert |
| `SANDBOX_RUNNER_REGISTRATION_GRPC_CERT_FILE` | *(required)* | Runner client cert (PEM) for registration mTLS |
| `SANDBOX_RUNNER_REGISTRATION_GRPC_KEY_FILE` | *(required)* | Runner client key (PEM) for registration mTLS |
| `SANDBOX_RUNNER_REGISTRATION_GRPC_SERVER_NAME` | *(empty)* | TLS name to verify on the API registration cert, and on volume object downloads from the API's host (defaults to host from `SANDBOX_RUNNER_API_GRPC_ADDR`) |
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
| `SANDBOX_RUNNER_DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker daemon endpoint used by the runner; must be `unix://` |
| `SANDBOX_RUNNER_ENABLE_CGROUPS` | `true` | Whether Docker resource limits are applied |
| `SANDBOX_RUNNER_DEFAULT_MEMORY_MB` | `512` | Default Docker memory limit per sandbox in megabytes |
| `SANDBOX_RUNNER_DEFAULT_CPU_PERCENT` | `100` | Default Docker CPU limit as a percentage of one core |
| `SANDBOX_RUNNER_DEFAULT_PIDS_MAX` | `256` | Default Docker process count limit per sandbox |
| `SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB` | `0` | Per-sandbox writable-layer quota in MB (`--storage-opt size=`). Effective only when the storage pool mounts successfully — see [Disk quotas](#disk-quotas). `0` means no quota. |
| `SANDBOX_RUNNER_DISK_QUOTA_POOL_SIZE_GB` | *(derived)* | Size of the xfs+prjquota storage pool backing the inner dockerd. Defaults to `ceil(SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB × SANDBOX_RUNNER_CAPACITY_TOTAL × 1.2 / 1024)` (per-sandbox quota times runner capacity, plus 20% headroom for sandbox image layers). Set explicitly to override. |
| `SANDBOX_RUNNER_DOCKER_INSECURE_REGISTRIES` | *(empty)* | Comma-separated insecure registries passed to dockerd |
| `SANDBOX_RUNNER_DOCKER_DNS_SERVERS` | *(unset)* | Comma-separated IPv4 resolvers for sandbox DNS, e.g. `1.1.1.1,8.8.8.8`. Unset, sandboxes resolve through the runner's own resolver. See [Sandbox DNS](security-model.md#sandbox-dns). |
| `SANDBOX_RUNNER_DOCKER_ACK_BR_NETFILTER_IS_LOADED` | `false` | On kernels before 5.3, where the runner cannot read `bridge-nf-call-iptables`, set `true` to confirm the host has `br_netfilter` loaded with the sysctl at `1`; the runner refuses to start otherwise. No effect on newer kernels. See [Guest-to-host and network](security-model.md#guest-to-host-and-network). |
| `SANDBOX_RUNNER_DOCKER_STORAGE_DRIVER` | *(dockerd default)* | Storage driver for the inner dockerd when disk quotas are off (e.g. `vfs` where nested overlayfs is unavailable). Ignored with quotas on, which always use `overlay2`. Read by `scripts/start-runner.sh`. |

### Firecracker runner backend config

These variables are parsed by the Firecracker runner entrypoint.

#### Firecracker slots

A Firecracker slot is one schedulable VM position on a runner. The runner creates one slot per unit of capacity; if capacity is `100`, the runner has slots `0` through `99`. A sandbox occupies one slot from create until stop/delete.

The runner sets the slot count once, at startup, before it listens. The count needs the golden snapshot's `mem_size_mib`, so when `snapshot_mem`, `snapshot_state` or `boot.json` is missing and `SANDBOX_RUNNER_FIRECRACKER_CREATE_SNAPSHOT_SCRIPT` is set, the runner builds the snapshot first. Before building, it runs the checks admission runs before a build (the Firecracker binaries, the template kernel and rootfs, who owns the kernel, and the release manifest) and checks that the create script and daemon binary exist. If any of these fails, the runner exits. A failed build is retried with backoff, from 1 s up to 30 s. A stop during the build stops the runner before it listens, and the create script cleans up after itself; a hard kill is cleaned up on the next start. The runner does not listen or register while it builds, so a new VM shows up in `/admin/runners` only once its snapshot exists; until then its progress is in its logs. With the snapshot missing and no create script, the runner exits. With `SANDBOX_RUNNER_CAPACITY_TOTAL` unset, the count is derived from host memory:

```text
slots = min(floor((MemTotal - 4 GiB) * ratio / (mem_size_mib + 64 MiB)), proxy ports, 16384)
```

`MemTotal` comes from `/proc/meminfo`; no cgroup memory limit is read. 4 GiB covers the runner, the OS and the shared snapshot file; 64 MiB is per-VM overhead. `ratio` is `SANDBOX_RUNNER_FIRECRACKER_MEMORY_RATIO` (default `1.0`). A set `SANDBOX_RUNNER_CAPACITY_TOTAL` may only lower the result. A higher value, a bad ratio, `MemTotal` that fits no guest, or a snapshot that does not match the runner's settings (see [boot.json](#golden-snapshot-boot-parameters-bootjson)) stops the runner before it listens. If a snapshot file goes missing while the runner runs, admission rebuilds the snapshot but keeps the slot count; restart the runner to size it from the new snapshot. When the proxy port range or the uplink limit binds, the runner logs it.

The data volume is not part of the slot count yet: a per-sandbox disk size has not been measured. Disk is checked per create instead: a create is refused, and the API tries another runner, when the volume has less than 5% free ([data volume](../internal/runner/runtime/firecracker.ee/README.md#data-volume)).

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
| `SANDBOX_RUNNER_FIRECRACKER_TEMPLATE_DIR` | `/srv/firecracker/template` | Directory containing the snapshot rootfs (`rootfs.ext4`) and the guest kernel (`vmlinux`). The kernel must be owned by root, readable by all and not writable by group or other, or admission fails |
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
| `SANDBOX_RUNNER_FIRECRACKER_CREATE_SNAPSHOT_SCRIPT` | _(empty)_ | Absolute path to `create-golden-snapshot.sh`. When set and any of `snapshot_mem`, `snapshot_state` or `boot.json` is missing, the runner runs it before listening, and Prepare runs it again if a file goes missing later (mem/state paths must share a directory). Production Firecracker hosts set this so the runner owns host-local snapshot creation. Empty = do not auto-create |
| `SANDBOX_RUNNER_FIRECRACKER_DAEMON_BIN` | `/srv/firecracker/bin/sandbox-daemon` | Host path to `sandbox-daemon` used for golden snapshot create and optional manifest checksum |
| `SANDBOX_RUNNER_FIRECRACKER_MEMORY_RATIO` | `1.0` | Multiplier on the memory-derived slot count; a finite number above 0. Above `1.0` can overcommit host memory, and nothing watches memory pressure |
| `SANDBOX_RUNNER_FIRECRACKER_VOLUME_BUILD_TIMEOUT` | `5m` | How long a [volume builder](../internal/runner/runtime/firecracker.ee/README.md#volume-builder) microVM may run before the runner ends it and the build fails with `timeout` |

The runner reports healthy only after `Prepare` has verified these assets, created the snapshot if needed, and passed an admission canary — see [the runtime README](../internal/runner/runtime/firecracker.ee/README.md#admission-prepare--ready).

#### Golden snapshot boot parameters (`boot.json`)

`create-golden-snapshot.sh` writes `boot.json` next to `snapshot_mem` and `snapshot_state`, recording what it sent to the Firecracker API: `vcpu_count`, `mem_size_mib`, `kernel_image_path`, `boot_args`, `rootfs_drive_path`, `volume_drives`, `guest_mac`, `guest_ip`, `host_tap_device_name`, `daemon_port`. Paths are as Firecracker sees them inside the jail. The runner has no configuration for most of these; it replays them when it cold boots a sandbox after a guest crash, so recovery stays faithful to how the snapshot was built.

- `kernel_image_path` and `rootfs_drive_path` are where the runner bind-mounts those assets into each jail (absolute, no `.`/`..` segments). Because the kernel path names a mount target, the runner also records the size and mtime of `TemplateDir/vmlinux` at create and refuses a later cold boot if they changed — roll a new kernel out by replacing the runner, not by rebuilding the template underneath it.
- Admission fails if `boot.json` contradicts the runner on `guest_ip`, `host_tap_device_name`, `daemon_port` or the gateway in `boot_args` (third field of the kernel `ip=` parameter; must equal the host address in `SANDBOX_RUNNER_FIRECRACKER_HOST_TAP_IP_CIDR`). These are baked into the guest, so a mismatch would otherwise yield sandboxes that never answer — except the gateway, where the guest still answers the proxy (and passes the canary) but has no egress.
- `volume_drives`: the read-only drives reserved for volumes ([runtime README](../internal/runner/runtime/firecracker.ee/README.md#volume-drives)). A `schema_version` 1 sidecar predates them and still loads if it lists none.
- Auto-create passes the runner's `GUEST_IP`, `HOST_TAP_IP_CIDR`, `HOST_TAP_DEVICE_NAME` and `DAEMON_PORT` to the script. When running it by hand, set the same variables. `MEM_MIB`, `VCPUS` and `VOLUME_DRIVES` (default 8, 0–15) are script-owned.
- With auto-create enabled, `SNAPSHOT_MEM_PATH`/`SNAPSHOT_STATE_PATH` must be the generated files or symlinks to them; an independent copy would keep serving an old snapshot under a `boot.json` describing the new one, and admission rejects that.
- The three files describe one build. A missing `boot.json` is an incomplete snapshot. With the create script set, the runner builds the set before it listens, and Prepare builds it again if a file goes missing later. Without the script, the runner exits before it listens.

#### Resource limits

The Firecracker runner does **not** read `SANDBOX_RUNNER_DEFAULT_MEMORY_MB`,
`SANDBOX_RUNNER_DEFAULT_CPU_PERCENT`, or `SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB`.
CPU and memory are fixed in the golden memory snapshot; disk capacity is fixed
by the template `rootfs.ext4` size. Change those by rebuilding the host snapshot
assets (see [`internal/runner/runtime/firecracker.ee/README.md`](../internal/runner/runtime/firecracker.ee/README.md)).

#### File descriptors

Every request the runner passes to a sandbox holds a few open files on the runner host while it runs. The per-sandbox limits, including the one for execution `DELETE`s, cap those requests, so the runner's open files grow with the number of sandboxes it runs, not with traffic. Set the runner's open-file limit (`LimitNOFILE` in its systemd unit) for a full runner: about 780 per slot with the default per-sandbox limit, plus headroom for connections to the runner itself, which stay open for up to 120 seconds when idle.

## Sandbox daemon

These variables are set inside each sandbox container and are typically baked into the sandbox image or passed through the runner.

| Variable | Default | Description |
| --- | --- | --- |
| `SANDBOX_DAEMON_LOG_LEVEL` | `info` | Minimum log severity (`debug`, `info`, `warn`, `error`; case-insensitive) |
| `SANDBOX_EXEC_MAX_EVENT_BYTES` | `16777216` | Max bytes of event history retained per execution (16 MiB); must be positive |
| `SANDBOX_EXEC_RETAIN` | `10m` | Duration to retain completed executions (Go [`time.ParseDuration`](https://pkg.go.dev/time#ParseDuration) syntax, e.g. `10m`, `1h`); must be positive |

## Code daemon

The `code-daemon` runs inside a code sandbox and is reached only by the runner. It refuses peers from its own network namespace. Its `GET /healthz` probe does not use a lane. Invalid values prevent startup. The probe returns `503` once the shared runtime fails to start, stops, or the daemon closes; the daemon does not restart the runtime, since that would discard shared state.

| Variable | Default | Description |
| --- | --- | --- |
| `CODE_DAEMON_LOG_LEVEL` | `info` | Minimum log severity (`debug`, `info`, `warn`, `error`; case-insensitive) |
| `CODE_DAEMON_LISTEN_ADDR` | `:8081` | Listen address |
| `CODE_DAEMON_MAX_CONCURRENCY` | `1` | Number of lanes (`1`–`32`). A lane runs one task, from upload to completion |
| `CODE_DAEMON_ALLOW_LOCAL_CLIENTS` | `false` | Accept peers from inside the sandbox. Local development only |
| `CODE_DAEMON_WORK_DIR` | `/work` | Parent of the task directories and working directory of the runtime. Created with mode `0700` if missing |
| `CODE_DAEMON_RUNTIMES_FILE` | `/etc/code-runtime/runtimes.json` | Runtime registry, shipped in the sandbox image |
| `CODE_DAEMON_RUNTIME` | `n8n-js` | Registry entry to run |
| `CODE_DAEMON_RUNTIME_LOG_WRITES` | `16` | Writes sampled from each of the runtime's stdout and stderr into the daemon log, at most 1 KiB each (up to `10000`). Later output is dropped. A sampled write is also dropped when the log queue is full |
| `CODE_DAEMON_MAX_INPUT_BYTES` | `67108864` | Max total bytes of a task's input files, including task.json and binary files (64 MiB; up to 1 GiB) |
| `CODE_DAEMON_MAX_OUTPUT_BYTES` | `67108864` | Max total bytes of the output files a task returns (64 MiB; up to 1 GiB) |
| `CODE_DAEMON_MAX_FILES` | `256` | Max input files and max output files a task returns (up to `4096`) |

Input and output file paths are at most 1024 bytes. A task's input files can create up to 4096 directories. The output limits apply to the files a task returns, found in up to 4096 directories; writes to the output directory are not capped. A task fails if its output directory holds a symlink or another entry that is not a regular file or a directory.

The runtime registry is a JSON object keyed by runtime name. Each entry has a `command`: the executable followed by its arguments. A missing file, invalid JSON, or a missing or empty `command` for the selected runtime prevents startup. For example:

```json
{
  "n8n-js": { "command": ["/usr/local/bin/node", "/opt/runtime/index.js"] }
}
```

The daemon starts that command once, as the shared runtime process, when it dispatches the first task. The runtime must report ready within 15 seconds. A command that cannot be run, such as a wrong executable path, is only detected then: it fails that task and the health probe.

## Metrics

The API and runner can each expose a Prometheus `/metrics` endpoint. Set `SANDBOX_API_METRICS_ENABLED=true` and/or `SANDBOX_RUNNER_METRICS_ENABLED=true` to enable. By default both serve it on the same HTTP port that serves their public API; the API can move it to a port of its own. The endpoint:

- Bypasses `X-Api-Key`, matching the n8n core operator model. Operators are expected to firewall the port it lands on or front it with a private LB; otherwise anyone reaching the listener can read the metrics.
- Uses the `sandbox_` namespace, with a `role` label (`api` or `runner`) on every metric so series from both binaries can live in one Prometheus.
- Bounds cardinality by labeling HTTP series with the route pattern (e.g. `/sandboxes/{id}/executions`), not the raw path, and with the method only if it is a standard one (`other` otherwise).

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
- `sandbox_http_requests_in_flight`, requests in progress, and `sandbox_http_requests_limited_total{limit}`, requests refused by the per-tenant (`tenant`, API) or per-sandbox (`sandbox`, runner) limits, execution `DELETE` ones included (both binaries). Like `sandbox_http_requests_total`, they only see requests that passed authentication. The gauge counts the whole process, not one tenant or sandbox; the counter shows how often each limit is reached.
- API: `sandbox_sandbox_operations_total{operation,result}` (`operation="cordon_delete"` counts attempts to delete a stopped sandbox on a cordoned runner instead of waking it), `sandbox_sandboxes_active`, `sandbox_runners_registered`, and `sandbox_create_placements_total{result}`, how each create found a runner (`first_choice`, `retried`, `exhausted`, `no_runner`). `retried` grows while the fleet is full but creates still succeed, so watch it alongside the failures. `sandbox_shared_volume_publishes_total{result}` with `sandbox_shared_volume_publish_duration_seconds`, shared volume publishes, `sandbox_shared_volume_builds_total{result}`, their image builds, and `sandbox_shared_volume_sweep_removals_total{state}`, what the sweep removed (see [observability.md](observability.md)).
- Runner: `sandbox_container_operations_total{operation,result}`, `sandbox_container_operation_duration_seconds{operation}`, `sandbox_containers_active`, plus `sandbox_guest_deaths_total`, sandbox guests that died on their own, `sandbox_recoveries_total{result}`, attempts to bring one back, and `sandbox_runner_cordoned`, `1` while the runner is cordoned.
- `sandbox_tls_certificate_expiry_timestamp_seconds{path}`, the expiry of each TLS certificate the process presents, by file (both binaries). See [observability.md](observability.md#tls-certificates).
- Firecracker runner: `sandbox_lifecycle_step_duration_seconds{operation,step}`, the per-step breakdown of a create, wake or recovery, `sandbox_slots_unwired`, slots whose network namespace is not yet built (zero in steady state), `sandbox_volume_mount_requests_total{result}`, guest volume mount requests by result, and `sandbox_volume_builds_total{result}` with `sandbox_volume_build_duration_seconds`, volume image builds. See [observability.md](observability.md).
- Both runners, set once at startup: `sandbox_host_memory_bytes`, `sandbox_data_volume_bytes` (size of the filesystem holding the data dir) and `sandbox_capacity_total` (the capacity the runner reports to the API; `0` is unlimited on Docker).
- Runner volume cache: `sandbox_volume_cache_bytes` (what the budget counts), `sandbox_volume_cache_entries`, `sandbox_volume_cache_pinned_entries` (held by a sandbox, never evicted), `sandbox_volume_cache_fetches_total{result}` (`hit`, `downloaded`, or the failure: `http`, `size`, `digest`, `timeout`, `full`, `io`), `sandbox_volume_cache_download_duration_seconds` (successful downloads), `sandbox_volume_cache_evictions_total`, `sandbox_volume_cache_unpacks_total{result}` (`success`, or the failure: `invalid`, `full`, `timeout`, `io`) and `sandbox_volume_cache_unpack_duration_seconds` (successful unpacks).
- Runner volumes, both runtimes: `sandbox_volumes_left_out_total{reason}`, optional volumes a create left out.
- Plus the standard `go_*` and `process_*` collectors.

## Disk quotas

When `SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB > 0`, the runner emits `--storage-opt size=Nm` on each sandbox so the inner dockerd caps that sandbox's writable layer. To make the flag enforce anything, `scripts/start-runner.sh` allocates a loopback xfs image (sized from `SANDBOX_RUNNER_DISK_QUOTA_POOL_SIZE_GB` — see the table above for how the default is derived), formats it, mounts it with `prjquota` at `/var/lib/docker`, and starts the inner dockerd with `--storage-driver=overlay2` against that mount. When `SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB` is unset/`0`, the pool is not created and dockerd uses its default storage with no per-sandbox enforcement. `start-runner.sh` reports a successful mount to the runner by exporting `SANDBOX_RUNNER_DISK_QUOTA_ACTIVE=true`; it is not an operator setting.

The image itself is not bounded by the mount it backs. Put it on a volume with a size limit, and keep `SANDBOX_RUNNER_DISK_QUOTA_POOL_SIZE_GB` within that limit. `SANDBOX_RUNNER_DISK_QUOTA_POOL_PATH` moves the image. The Helm chart does this for you; see the chart README section 'Disk Quotas'.

**Host kernel requirement:** the runner container's host kernel must be built with `CONFIG_XFS_QUOTA` (=y or =m). Every mainstream Linux distro kernel ships with this enabled. The notable exception is **Docker Desktop's linuxkit kernel** on macOS, which omits it — on that host the loopback mount fails and the runner logs `disk quota enforcement: DISABLED` and continues without per-sandbox enforcement. Sandboxes still run, just without writable-layer caps. To check a node: `zcat /proc/config.gz | grep XFS_QUOTA` or `cat /boot/config-$(uname -r) | grep XFS_QUOTA`.
