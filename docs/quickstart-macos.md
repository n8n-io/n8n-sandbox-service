# Quickstart: macOS

This guide covers running the n8n Sandbox Service on macOS for local development. Sysbox is not available on macOS, so runners use `--privileged` mode instead.

> **Note:** Privileged mode is less secure than sysbox-runc. For production deployments, use [Linux with sysbox](quickstart-linux.md).

## Contents

- [Prerequisites](#prerequisites)
- [Pull images](#pull-images)
- [Set up mTLS certificates](#set-up-mtls-certificates)
- [Start the API](#start-the-api)
- [Start a runner](#start-a-runner)
- [Verify](#verify)
- [Limitations](#limitations)
- [Next steps](#next-steps)

## Prerequisites

See [prerequisites.md](prerequisites.md) for shared requirements.

On macOS, Docker Desktop runs a Linux VM under the hood, which is how Docker-in-Docker works in privileged mode. No additional runtime installation is needed.

## Pull images

Pull the three service images from Docker Hub:

```bash
docker pull n8nio/n8n-sandbox-api:latest
docker pull n8nio/n8n-sandbox-runner:latest
docker pull n8nio/n8n-sandbox:latest
```

## Set up mTLS certificates

The API and runners communicate over gRPC with mutual TLS (mTLS). The repository includes a bootstrap script that generates a private CA and all leaf certificates:

```bash
./scripts/bootstrap-local-mtls.sh
```

This writes PEM files to `.tls/` in the repository root. Set `SANDBOX_TLS_REGEN=1` to regenerate existing certificates.

See the [Linux quickstart](quickstart-linux.md#set-up-mtls-certificates) for details on the certificate roles.

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

See [configuration.md](configuration.md) for the full list of API environment variables.

## Start a runner

```bash
docker run -d \
  --name sandbox-runner-1 \
  --privileged \
  -v $(pwd)/.tls:/tls:ro \
  -e SANDBOX_RUNNER_API_KEYS=<runner-api-key> \
  -e SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE=localhost:5000/n8n-sandbox:latest \
  -e SANDBOX_RUNNER_DOCKER_INSECURE_REGISTRIES=localhost:5000 \
  -e SANDBOX_RUNNER_API_GRPC_ADDR=host.docker.internal:9090 \
  -e SANDBOX_RUNNER_REGISTRATION_TOKEN=<shared-registration-token> \
  -e SANDBOX_RUNNER_HTTP_BASE_URL=http://sandbox-runner-1:8080 \
  -e SANDBOX_RUNNER_CONTROL_GRPC_LISTEN_ADDR=:9091 \
  -e SANDBOX_RUNNER_CONTROL_GRPC_ADVERTISE_ADDR=sandbox-runner-1:9091 \
  -e SANDBOX_RUNNER_REGISTRATION_GRPC_CA_FILE=/tls/ca.crt \
  -e SANDBOX_RUNNER_REGISTRATION_GRPC_CERT_FILE=/tls/grpc-client.crt \
  -e SANDBOX_RUNNER_REGISTRATION_GRPC_KEY_FILE=/tls/grpc-client.key \
  -e SANDBOX_RUNNER_CONTROL_GRPC_TLS_CERT_FILE=/tls/control-grpc-server.crt \
  -e SANDBOX_RUNNER_CONTROL_GRPC_TLS_KEY_FILE=/tls/control-grpc-server.key \
  -e SANDBOX_RUNNER_CONTROL_GRPC_TLS_CLIENT_CA_FILE=/tls/ca.crt \
  n8nio/n8n-sandbox-runner:latest
```

The key difference from Linux is `--privileged` instead of `--runtime=sysbox-runc`. This grants the runner container full host capabilities, which is acceptable for local development but not recommended for production.

On macOS, `host.docker.internal` resolves to the Docker Desktop host, which is used here to reach the API's gRPC port.

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

## Limitations

- **No sysbox:** macOS does not support sysbox-runc. Runners use `--privileged` mode, which provides weaker isolation than sysbox.

For production deployments, use [Linux with sysbox](quickstart-linux.md).

## Next steps

- [Configuration](configuration.md) — tune environment variables for API, Runner, and Daemon
- [Development guide](development.md) — build from source, run tests, use the playground
