# Quickstart: Linux

Deploy the API and a Docker runner on one Linux host, with [sysbox-runc](https://github.com/nestybox/sysbox) providing unprivileged Docker-in-Docker for the runner.

## Prerequisites

- Docker >= 24.
- A distribution sysbox supports: Ubuntu 18–24 or Debian 10–11 on amd64/arm64 with kernel > 5.19; other distributions need sysbox built from source (see the [compatibility matrix](https://github.com/nestybox/sysbox/blob/master/docs/distro-compat.md)).
- Not an immutable-rootfs distribution (Talos, Bottlerocket, Flatcar, Fedora CoreOS): sysbox must write to the node filesystem. On Kubernetes, use the chart's [privileged isolation](quickstart-k8s.md#immutable-rootfs-distributions-privileged-isolation) there.

## Install sysbox

The setup script installs sysbox v0.7.1 (v0.7.0 is also supported):

```bash
curl -fsSL -o setup-sysbox.sh https://raw.githubusercontent.com/n8n-io/n8n-sandbox-service/refs/heads/main/scripts/setup-sysbox.sh
chmod +x setup-sysbox.sh
./setup-sysbox.sh --dry-run   # check prerequisites only
./setup-sysbox.sh
docker info --format '{{json .Runtimes}}' | jq '.["sysbox-runc"]'   # verify
```

## Start the services

```bash
curl -fsSL -o compose.yaml https://raw.githubusercontent.com/n8n-io/n8n-sandbox-service/refs/heads/main/docs/examples/compose.linux.yaml
curl -fsSL -o .env.example https://raw.githubusercontent.com/n8n-io/n8n-sandbox-service/refs/heads/main/docs/examples/.env.example
cp .env.example .env    # then replace the placeholder values
docker compose up -d
curl http://localhost:8080/healthz
```

On first run the `tls-init` container generates a private CA and leaf certificates into `.tls/`; later runs reuse them. Delete `.tls/` and restart to regenerate. All other settings are in [configuration.md](configuration.md).

## Certificates

The API and runners talk mTLS on both gRPC and HTTP. `tls-init` issues four leaf certificates into per-service directories, so each container mounts only its own material:

| File | Role | Used by |
| --- | --- | --- |
| `.tls/api/grpc-server.*` | Registration gRPC server | API, listens on :9090 |
| `.tls/api/control-grpc-api-client.*` | SandboxControl client | API, dials runner :9091 and :8080 |
| `.tls/runner/grpc-client.*` | Registration gRPC client | Runner, dials API :9090 |
| `.tls/runner/control-grpc-server.*` | SandboxControl server | Runner, serves :9091 and HTTPS :8080 |

The runner's HTTP listener reuses the SandboxControl pair, so the host in `SANDBOX_RUNNER_HTTP_BASE_URL` must be one of that certificate's SANs. The role-to-env-var mapping is in [cert-manager-k8s.md](cert-manager-k8s.md).

## Next steps

- [Configuration](configuration.md) — tune API, runner and daemon
- [Development guide](development.md) — build from source, run tests, use the playground
