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

## Documentation

See [docs/](docs/README.md) for the full documentation, including:

- [Linux quickstart](docs/quickstart-linux.md) — production deployment with sysbox-runc
- [macOS quickstart](docs/quickstart-macos.md) — local development with privileged containers
- [Configuration reference](docs/configuration.md) — all environment variables
- [Development guide](docs/development.md) — building, testing, SDK, playground

## API Usage

All endpoints except `/healthz` require `X-Api-Key`.

### Create a sandbox

```bash
curl -s -X POST http://localhost:8080/sandboxes \
  -H "X-Api-Key: test" | jq
```

### Run a command

```bash
curl -s -X POST http://localhost:8080/sandboxes/<id>/executions \
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

See [docs/configuration.md](docs/configuration.md) for environment variables for the API, Runner, and Sandbox daemon.

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

See [docs/development.md](docs/development.md) for building from source, running tests, the playground, SDK development, and code formatting.
