# Quickstart: Linux

This guide covers deploying the n8n Sandbox Service on a Linux machine using [sysbox-runc](https://github.com/nestybox/sysbox) for secure Docker-in-Docker isolation.

## Contents

- [Prerequisites](#prerequisites)
- [Install sysbox](#install-sysbox)
- [Set up mTLS certificates](#set-up-mtls-certificates)
- [Start the services](#start-the-services)
- [Verify](#verify)
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

## Set up mTLS certificates

The API and runners communicate over gRPC with mutual TLS (mTLS). You need to generate or provide the following certificates signed by a shared CA:

| Certificate | Type | Used by |
|---|---|---|
| Registration gRPC server | `server auth` | API (listens on :9090) |
| Registration gRPC client | `client auth` | Runner (dials API :9090) |
| SandboxControl gRPC server | `server auth` | Runner (listens on :9091) |
| SandboxControl gRPC client | `client auth` | API (dials runner :9091) |

For a quick local setup, a bootstrap script is available that generates a private CA and all leaf certificates:

```bash
curl -fsSL -o bootstrap-local-mtls.sh https://raw.githubusercontent.com/n8n-io/n8n-sandbox-service/refs/heads/main/scripts/bootstrap-local-mtls.sh
chmod +x bootstrap-local-mtls.sh
./bootstrap-local-mtls.sh
```

This writes PEM files to `.tls/` in the repository root. Set `SANDBOX_TLS_REGEN=1` to regenerate existing certificates.

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

See [configuration.md](configuration.md) for the full list of environment variables.

## Verify

Check the API health endpoint:

```bash
curl http://localhost:8080/healthz
```

## Next steps

- [Configuration](configuration.md) — tune environment variables for API, Runner, and Daemon
- [Development guide](development.md) — build from source, run tests, use the playground
