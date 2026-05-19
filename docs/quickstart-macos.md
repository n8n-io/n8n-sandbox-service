# Quickstart: macOS

This guide covers running the n8n Sandbox Service on macOS for local development. Sysbox is not available on macOS, so runners use `--privileged` mode instead.

> **Note:** Privileged mode is less secure than sysbox-runc. For production deployments, use [Linux with sysbox](quickstart-linux.md).

## Contents

- [Prerequisites](#prerequisites)
- [Set up mTLS certificates](#set-up-mtls-certificates)
- [Start the services](#start-the-services)
- [Verify](#verify)
- [Limitations](#limitations)
- [Next steps](#next-steps)

## Prerequisites

See [prerequisites.md](prerequisites.md) for shared requirements.

On macOS, Docker Desktop runs a Linux VM under the hood, which is how Docker-in-Docker works in privileged mode. No additional runtime installation is needed.

## Set up mTLS certificates

The API and runners communicate over gRPC with mutual TLS (mTLS). A bootstrap script is available that generates a private CA and all leaf certificates:

```bash
curl -fsSL -o bootstrap-local-mtls.sh https://raw.githubusercontent.com/n8n-io/n8n-sandbox-service/refs/heads/main/scripts/bootstrap-local-mtls.sh
chmod +x bootstrap-local-mtls.sh
./bootstrap-local-mtls.sh
```

This writes PEM files to `.tls/` in the repository root. Set `SANDBOX_TLS_REGEN=1` to regenerate existing certificates.

See the [Linux quickstart](quickstart-linux.md#set-up-mtls-certificates) for details on the certificate roles.

## Start the services

Download the macOS compose file and environment template:

```bash
curl -fsSL -o compose.yaml https://raw.githubusercontent.com/n8n-io/n8n-sandbox-service/refs/heads/main/docs/examples/compose.macos.yaml
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

## Limitations

- **No sysbox:** macOS does not support sysbox-runc. Runners use `--privileged` mode, which provides weaker isolation than sysbox.

For production deployments, use [Linux with sysbox](quickstart-linux.md).

## Next steps

- [Configuration](configuration.md) — tune environment variables for API, Runner, and Daemon
- [Development guide](development.md) — build from source, run tests, use the playground
