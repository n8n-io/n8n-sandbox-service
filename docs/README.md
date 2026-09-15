# Documentation

## Getting started

| Platform | Use case | Guide |
| --- | --- | --- |
| Linux | Production, Docker runner with Sysbox isolation | [quickstart-linux.md](quickstart-linux.md) |
| Linux + KVM | Production, Firecracker runner | [quickstart-firecracker-linux.md](quickstart-firecracker-linux.md) |
| Kubernetes | Production cluster deployment (Helm) | [quickstart-k8s.md](quickstart-k8s.md) · [chart README](../charts/n8n-sandbox-service/README.md) |
| macOS | Local development, privileged containers | [quickstart-macos.md](quickstart-macos.md) |

## Reference

- [Architecture](architecture.md) — components, communication, data flows, storage
- [Security model](security-model.md) — trust boundaries, what is enforced where, non-guarantees
- [Configuration](configuration.md) — every environment variable for API, runner and daemon; metrics; disk quotas
- [REST API](API.md) — endpoint reference and error contract
- [TypeScript SDK](../sdk/README.md) — client library
- Runtime internals: [Docker/Sysbox](../internal/runner/runtime/docker/README.md) · [Firecracker](../internal/runner/runtime/firecracker.ee/README.md)

## Operations

- [Observability](observability.md) — trace ids, canonical log events, how to query them
- [Performance baseline](performance.md) — recorded numbers and how to reproduce them
- [mTLS on Kubernetes with cert-manager](cert-manager-k8s.md) — certificate roles and the env vars they map to
- [Debugging gRPC with grpcurl](grpcurl-debug.md)

## Development and release

- [Development guide](development.md) — building, running locally, tests, playground, SDK, formatting
- [Release process](RELEASE.md) — service and SDK release pipelines
- [Firecracker golden-build bundle](../BUNDLE.md) — tarball contract and rollout order for Firecracker hosts
- [End-to-end tests](../e2e/README.md) · [Benchmarks](../benchmarks/README.md)
