# n8n Sandbox Service

The n8n Sandbox Service provides isolated execution environments via a REST API. Each sandbox is a Debian-based Docker container managed by an in-container Docker daemon, with a per-sandbox HTTP daemon that handles exec and file operations.

## Documentation

See [docs/](docs/README.md) for the full documentation, including:

- [Linux quickstart](docs/quickstart-linux.md) — production deployment with sysbox-runc
- [macOS quickstart](docs/quickstart-macos.md) — local development with privileged containers
- [k8s quickstart](docs/quickstart-k8s.md) - production k8s deployment with sysbox-runc
- [Configuration reference](docs/configuration.md) — all environment variables
- [Development guide](docs/development.md) — building, testing, SDK, playground
- [REST API reference](docs/API.md) — endpoint reference

## Runtime Model

- The public API runs in a dedicated `n8n-sandbox-service-api` container.
- One or more `n8n-sandbox-service-runner-dind` containers run Docker-in-Docker and manage sandbox lifecycles (the local script starts two so you can exercise round-robin placement).
- The runner container is expected to run with `sysbox-runc`.
- Sandboxes are started from a separate Debian sandbox image referenced by `SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE`.
- The API forwards sandbox and image requests to the runner; the runner talks to sandbox daemons over the inner Docker bridge on port `8081`.

## Failure Behavior

- API restarts: sandbox IDs remain valid. Once API is back, existing sandboxes continue working.
- Runner stops/dies: sandboxes on that runner become unavailable. When a runner returns, previously assigned sandboxes are not guaranteed to be recoverable and should be treated as lost.
- Sandbox container exits (for example OOM): Docker restart policy restarts it; the same sandbox ID remains on the same runner.

## Disk quotas

When `SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB > 0`, the runner emits `--storage-opt size=Nm` on each sandbox so the inner dockerd caps that sandbox's writable layer. To make the flag enforce anything, `scripts/start-runner.sh` allocates a loopback xfs image (sized from `SANDBOX_RUNNER_DISK_QUOTA_POOL_SIZE_GB` — see the table above for how the default is derived), formats it, mounts it with `prjquota` at `/var/lib/docker`, and starts the inner dockerd with `--storage-driver=overlay2` against that mount. When `SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB` is unset/`0`, the pool is not created and dockerd uses its default storage with no per-sandbox enforcement.

**Host kernel requirement:** the runner container's host kernel must be built with `CONFIG_XFS_QUOTA` (=y or =m). Every mainstream Linux distro kernel ships with this enabled. The notable exception is **Docker Desktop's linuxkit kernel** on macOS, which omits it — on that host the loopback mount fails and the runner logs `disk quota enforcement: DISABLED` and continues without per-sandbox enforcement. Sandboxes still run, just without writable-layer caps. To check a node: `zcat /proc/config.gz | grep XFS_QUOTA` or `cat /boot/config-$(uname -r) | grep XFS_QUOTA`.

## Runner registration gRPC (mTLS)

**Local Docker Compose:** `make up` runs `scripts/bootstrap-mtls.sh`, which writes a private CA plus leaf certs into `.tls/` at the repo root. If those files already exist, bootstrap does nothing unless you set `SANDBOX_TLS_REGEN=1`. Compose always mounts `.tls` via `compose.tls.yaml` and sets required `SANDBOX_*_GRPC_TLS_*` variables.

**Kubernetes:** Use your own CA (often `cert-manager`). Mount PEMs from `Certificate` secrets and set the same env vars as in the tables above. See [`docs/cert-manager-k8s.md`](docs/cert-manager-k8s.md).

**Registration vs lifecycle:** Runners dial the API over gRPC for registration. Sandbox **create/delete** use the runner’s **SandboxControl** gRPC address when advertised; **exec/files** and other sandbox routes are still proxied over HTTP.

**Debugging gRPC:** See [`docs/grpcurl-debug.md`](docs/grpcurl-debug.md).

**Security FAQ (draft):** See [`docs/security-faq.md`](docs/security-faq.md).

**Weak points + hardening plan (draft):** See [`docs/security-weak-points-and-hardening.md`](docs/security-weak-points-and-hardening.md).

**Bearer token:** Still required in metadata (`Authorization: Bearer …`) in addition to mTLS for registration.

**Why this matters for trust:** mTLS ties the registration gRPC stream to a client certificate issued by your CA. A random host on the network cannot complete TLS to the API’s registry listener, so it cannot open a stream and inject a fake `runner_id` / `http_base_url` into placement. Legitimate runners still prove possession of the registration `Bearer` token in metadata. Together, only workloads you issued credentials to can show up in the runner registry. Optional mTLS on **SandboxControl** ensures only your API (presenting the control client cert) can ask a runner to create or delete sandboxes over gRPC; runners still accept the same `X-Api-Key` on that RPC as on HTTP. HTTP proxy traffic continues to use `X-Api-Key` and stored routing.

**Rotation:** The API and runner reload leaf cert/key PEMs from disk when the files change (next TLS handshake), so in Kubernetes `cert-manager` (or any process that writes renewed files into the mounted paths) can rotate leaves without restarting pods. Local bootstrap issues long‑lived dev certs; they do not auto‑renew on a timer. When they eventually expire—or whenever you want fresh material for local Compose—delete `.tls/` or set `SANDBOX_TLS_REGEN=1` and run `scripts/run-locally.sh` so the local SANs are regenerated correctly (then restart containers or wait for the next TLS handshake so reloaded leaf material is used). Rotating the CA both sides trust means updating the mounted CA PEMs and typically rolling workloads.
