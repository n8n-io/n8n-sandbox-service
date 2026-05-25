# Quickstart: macOS

This guide covers running the n8n Sandbox Service on macOS for local development. Sysbox is not available on macOS, so runners use `--privileged` mode instead.

> **Note:** Privileged mode is less secure than sysbox-runc. For production deployments, use [Linux with sysbox](quickstart-linux.md).

## Contents

- [Prerequisites](#prerequisites)
- [Start the services](#start-the-services)
- [Verify](#verify)
- [Limitations](#limitations)
- [Next steps](#next-steps)

## Prerequisites

See [prerequisites.md](prerequisites.md) for shared requirements.

On macOS, Docker Desktop runs a Linux VM under the hood, which is how Docker-in-Docker works in privileged mode. No additional runtime installation is needed.

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

On first run, a `tls-init` container automatically generates mTLS certificates into `.tls/`. Subsequent runs skip generation if certificates already exist. Delete `.tls/` and restart to regenerate.

See [configuration.md](configuration.md) for the full list of environment variables. See the [Linux quickstart](quickstart-linux.md#mtls-certificate-details) for details on the certificate roles.

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
