# Quickstart: Linux

This guide covers deploying the n8n Sandbox Service on a Linux machine using [sysbox-runc](https://github.com/nestybox/sysbox) for secure Docker-in-Docker isolation.

## Contents

- [Prerequisites](#prerequisites)
- [Install sysbox](#install-sysbox)
- [Start the services](#start-the-services)
- [Verify](#verify)
- [mTLS certificate details](#mtls-certificate-details)
- [Next steps](#next-steps)

## Prerequisites

See [prerequisites.md](prerequisites.md) for shared requirements.

Linux deployments additionally require **sysbox-runc** installed on the host. Sysbox provides secure, unprivileged Docker-in-Docker — runners create sandbox containers inside themselves without needing `--privileged`.

## Install sysbox

An automated setup script is available that installs sysbox v0.7.0.

1. **Download the setup script:**

```bash
curl -fsSL -o setup-sysbox.sh https://raw.githubusercontent.com/n8n-io/n8n-sandbox-service/refs/heads/main/scripts/setup-sysbox.sh
chmod +x setup-sysbox.sh
```

2. **Check prerequisites without installing (dry run):**

```bash
./setup-sysbox.sh --dry-run
```

3. **Install sysbox:**

```bash
./setup-sysbox.sh
```

4. **Verify** sysbox is registered as a Docker runtime:

```bash
docker info --format '{{json .Runtimes}}' | jq '.["sysbox-runc"]'
```

## Start the services

Download the production compose file and environment template:

```bash
curl -fsSL -o compose.yaml https://raw.githubusercontent.com/n8n-io/n8n-sandbox-service/refs/heads/main/docs/examples/compose.linux.yaml
curl -fsSL -o .env.example https://raw.githubusercontent.com/n8n-io/n8n-sandbox-service/refs/heads/main/docs/examples/.env.example
```

Copy the environment template and fill in your values:

```bash
cp .env.example .env
# Edit .env and replace the placeholder values
```

Start the API and runner:

```bash
docker compose up -d
```

On first run, a `tls-init` container automatically generates mTLS certificates into `.tls/`. Subsequent runs skip generation if certificates already exist. Delete `.tls/` and restart to regenerate.

See [configuration.md](configuration.md) for the full list of environment variables.

## Verify

Check the API health endpoint:

```bash
curl http://localhost:8080/healthz
```

## mTLS certificate details

The API and runners communicate with mutual TLS (mTLS), over both gRPC and HTTP. The `tls-init` init container generates a private CA and the following leaf certificates on first startup:

| Certificate | Type | Used by |
|---|---|---|
| Registration gRPC server | `server auth` | API (listens on :9090) |
| Registration gRPC client | `client auth` | Runner (dials API :9090) |
| SandboxControl gRPC server | `server auth` | Runner (listens on :9091, and on :8080 for HTTP) |
| SandboxControl gRPC client | `client auth` | API (dials runner :9091, and :8080 for exec and file traffic) |

The runner's HTTP listener reuses the SandboxControl pair rather than having its own, so the host in `SANDBOX_RUNNER_HTTP_BASE_URL` must be one of that certificate's SANs. The listener only serves TLS, so an `http://` value left over from an older deployment is upgraded to `https://` and logged as a warning at startup.

Certificates are organized into per-service directories (`.tls/api/` and `.tls/runner/`) so each service only has access to its own material.

## Next steps

- [Configuration](configuration.md) — tune environment variables for API, Runner, and Daemon
- [Development guide](development.md) — build from source, run tests, use the playground
