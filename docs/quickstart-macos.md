# Quickstart: macOS

Run the service locally on macOS. Sysbox is not available there, so the runner runs `--privileged` inside Docker Desktop's Linux VM. This is a development setup, not an isolation boundary; for production use [Linux with sysbox](quickstart-linux.md).

## Prerequisites

Docker Desktop (Docker >= 24). Nothing else to install.

## Start the services

```bash
curl -fsSL -o compose.yaml https://raw.githubusercontent.com/n8n-io/n8n-sandbox-service/refs/heads/main/docs/examples/compose.macos.yaml
curl -fsSL -o .env.example https://raw.githubusercontent.com/n8n-io/n8n-sandbox-service/refs/heads/main/docs/examples/.env.example
cp .env.example .env    # then replace the placeholder values
docker compose up -d
curl http://localhost:8080/healthz
```

On first run the `tls-init` container generates mTLS certificates into `.tls/`; delete the directory and restart to regenerate. The certificate roles are described in the [Linux quickstart](quickstart-linux.md#certificates).

## Limitations

- Privileged runner instead of sysbox: weaker isolation.
- Docker Desktop's kernel lacks `CONFIG_XFS_QUOTA`, so per-sandbox [disk quotas](configuration.md#disk-quotas) are not enforced.

## Next steps

- [Configuration](configuration.md)
- [Development guide](development.md) — `make up` runs the same stack from a checkout, with two runners
