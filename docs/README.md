# n8n Sandbox Service — Documentation

The n8n Sandbox Service provides isolated execution environments via a REST API. Each sandbox is a Debian-based Docker container managed by an in-container Docker daemon, with a per-sandbox HTTP daemon that handles exec and file operations.

## Getting Started

| Platform | Use case | Guide |
|----------|----------|-------|
| **Linux** | Production (sysbox-runc isolation) | [quickstart-linux.md](quickstart-linux.md) |
| **macOS** | Local development (privileged containers) | [quickstart-macos.md](quickstart-macos.md) |
| **Kubernetes** | Production cluster deployment | _Coming soon_ |

> Both platform guides share common [prerequisites](prerequisites.md).

## Reference

- [Configuration](configuration.md) — environment variables for API, Runner, and Daemon
- [gRPC mTLS](../README.md#runner-registration-grpc-mtls) — certificate bootstrap, rotation, and trust model
- [REST API](API.md) — endpoint reference
- [TypeScript SDK](../sdk/README.md) — client library

## Development

- [Development guide](development.md) — building, testing, playground, formatting

## Operations

- [Debugging gRPC with grpcurl](grpcurl-debug.md)
- [cert-manager on Kubernetes](cert-manager-k8s.md)
