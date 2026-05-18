# Quickstart: Linux

This guide covers deploying the n8n Sandbox Service on a Linux machine using [sysbox-runc](https://github.com/nestybox/sysbox) for secure Docker-in-Docker isolation.

## Contents

- [Prerequisites](#prerequisites)
- [Install sysbox](#install-sysbox)
- [Pull images](#pull-images)
- [Set up mTLS certificates](#set-up-mtls-certificates)
- [Start the API](#start-the-api)
- [Start a runner](#start-a-runner)
- [Verify](#verify)
- [Next steps](#next-steps)

## Prerequisites

See [prerequisites.md](prerequisites.md) for shared requirements.

Linux deployments additionally require **sysbox-runc** installed on the host. Sysbox provides secure, unprivileged Docker-in-Docker — runners create sandbox containers inside themselves without needing `--privileged`.

## Install sysbox

The repository includes an automated setup script at `scripts/setup-sysbox.sh` that installs sysbox v0.7.0.

**Supported platforms:**

| Distribution | Versions |
|---|---|
| Ubuntu | 18, 20, 22, 24 |
| Debian | 10, 11 |

Other distributions are also supported but require building sysbox from source. See the [sysbox distribution compatibility matrix](https://github.com/nestybox/sysbox/blob/master/docs/distro-compat.md) for the full list.

**Requirements:**
- Architecture: amd64 or arm64
- Kernel: > 5.19
- Docker must be installed first

**Check prerequisites without installing (dry run):**

```bash
./scripts/setup-sysbox.sh --dry-run
```

**Install sysbox:**

```bash
./scripts/setup-sysbox.sh
```

After installation, verify sysbox is registered as a Docker runtime:

```bash
docker info --format '{{json .Runtimes}}' | jq '.["sysbox-runc"]'
```

## Pull images

Pull the three service images from Docker Hub:

```bash
docker pull n8nio/n8n-sandbox-api:latest
docker pull n8nio/n8n-sandbox-runner:latest
docker pull n8nio/n8n-sandbox:latest
```

## Set up mTLS certificates

The API and runners communicate over gRPC with mutual TLS (mTLS). You need to generate or provide the following certificates signed by a shared CA:

| Certificate | Type | Used by |
|---|---|---|
| Registration gRPC server | `server auth` | API (listens on :9090) |
| Registration gRPC client | `client auth` | Runner (dials API :9090) |
| SandboxControl gRPC server | `server auth` | Runner (listens on :9091) |
| SandboxControl gRPC client | `client auth` | API (dials runner :9091) |

For a quick local setup, the repository includes a bootstrap script that generates a private CA and all leaf certificates:

```bash
./scripts/bootstrap-local-mtls.sh
```

This writes PEM files to `.tls/` in the repository root. Set `SANDBOX_TLS_REGEN=1` to regenerate existing certificates.

## Start the API

```bash
docker run -d \
  --name sandbox-api \
  -p 8080:8080 \
  -p 9090:9090 \
  -v $(pwd)/.tls:/tls:ro \
  -e SANDBOX_API_KEYS=<your-api-key> \
  -e SANDBOX_API_RUNNER_REGISTRATION_TOKEN=<shared-registration-token> \
  -e SANDBOX_API_RUNNER_API_KEY=<runner-api-key> \
  -e SANDBOX_API_GRPC_TLS_CERT_FILE=/tls/grpc-server.crt \
  -e SANDBOX_API_GRPC_TLS_KEY_FILE=/tls/grpc-server.key \
  -e SANDBOX_API_GRPC_TLS_CLIENT_CA_FILE=/tls/ca.crt \
  -e SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CA_FILE=/tls/ca.crt \
  -e SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CERT_FILE=/tls/control-grpc-api-client.crt \
  -e SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_KEY_FILE=/tls/control-grpc-api-client.key \
  n8nio/n8n-sandbox-api:latest
```

- Port `8080` — public REST API
- Port `9090` — private gRPC for runner registration

See [configuration.md](configuration.md) for the full list of API environment variables.

## Start a runner

```bash
docker run -d \
  --name sandbox-runner-1 \
  --runtime=sysbox-runc \
  -v $(pwd)/.tls:/tls:ro \
  -e SANDBOX_RUNNER_API_KEYS=<runner-api-key> \
  -e SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE=localhost:5000/n8n-sandbox:latest \
  -e SANDBOX_RUNNER_DOCKER_INSECURE_REGISTRIES=localhost:5000 \
  -e SANDBOX_RUNNER_API_GRPC_ADDR=<api-host>:9090 \
  -e SANDBOX_RUNNER_REGISTRATION_TOKEN=<shared-registration-token> \
  -e SANDBOX_RUNNER_HTTP_BASE_URL=http://<runner-host>:8080 \
  -e SANDBOX_RUNNER_CONTROL_GRPC_LISTEN_ADDR=:9091 \
  -e SANDBOX_RUNNER_CONTROL_GRPC_ADVERTISE_ADDR=<runner-host>:9091 \
  -e SANDBOX_RUNNER_REGISTRATION_GRPC_CA_FILE=/tls/ca.crt \
  -e SANDBOX_RUNNER_REGISTRATION_GRPC_CERT_FILE=/tls/grpc-client.crt \
  -e SANDBOX_RUNNER_REGISTRATION_GRPC_KEY_FILE=/tls/grpc-client.key \
  -e SANDBOX_RUNNER_CONTROL_GRPC_TLS_CERT_FILE=/tls/control-grpc-server.crt \
  -e SANDBOX_RUNNER_CONTROL_GRPC_TLS_KEY_FILE=/tls/control-grpc-server.key \
  -e SANDBOX_RUNNER_CONTROL_GRPC_TLS_CLIENT_CA_FILE=/tls/ca.crt \
  n8nio/n8n-sandbox-runner:latest
```

The `--runtime=sysbox-runc` flag is what enables secure Docker-in-Docker. Sysbox intercepts container syscalls to provide VM-like isolation without the overhead of full virtualization.

Replace `<api-host>` and `<runner-host>` with the actual hostnames or IP addresses. The runner must be able to reach the API on port 9090, and the API must be able to reach the runner on its advertised HTTP and gRPC ports.

If using an insecure (HTTP) registry, set `SANDBOX_RUNNER_DOCKER_INSECURE_REGISTRIES` to allow the runner's inner Docker daemon to pull from it.

See [configuration.md](configuration.md) for the full list of runner environment variables.

## Verify

Check the API health endpoint:

```bash
curl http://localhost:8080/healthz
```

Create a test sandbox:

```bash
curl -s -X POST http://localhost:8080/sandboxes \
  -H "X-Api-Key: <your-api-key>" | jq
```

Run a command in the sandbox:

```bash
curl -s -X POST http://localhost:8080/sandboxes/<id>/executions \
  -H "X-Api-Key: <your-api-key>" \
  -H "Content-Type: application/json" \
  -d '{"command": "echo hello world"}'
```

## Next steps

- [Configuration](configuration.md) — tune environment variables for API, Runner, and Daemon
- [Development guide](development.md) — build from source, run tests, use the playground
