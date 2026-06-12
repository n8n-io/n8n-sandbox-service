# Architecture

The n8n Sandbox Service provides isolated, on-demand execution environments via a REST API. Each sandbox is a Debian-based Docker container with a per-container HTTP daemon that handles command execution and file operations. The service is designed for horizontal scalability: runners register dynamically with a central API gateway, which routes client requests to the appropriate runner and sandbox.

## System Overview

```text
                    ┌────────────────────────────────────────────────────────────┐
                    │              API Gateway (Go app in a container)           │
                    │                                                            │
  Client ── REST ─▶ │  ┌──────────┐                                              │
  (SDK)  (X-Api-Key)│  │ HTTP     │    ┌──────────────────┐                      │
                    │  │ Server   │    │  Idle Sweeper    │                      │
                    │  │ (:8080)  │    └──────────────────┘                      │
                    │  └────┬─────┘                                              │
                    │       │         ┌──────────────────┐  ┌─────────────────┐  │
                    │  ┌────▼─────┐   │ Registration     │  │ Control         │  │
                    │  │  Store   │   │ gRPC Server      │  │ gRPC Client     │  │
                    │  │ (SQLite) │   │ (:9090)          │  │                 │  │
                    │  └──────────┘   └────────▲─────────┘  └────────┬────────┘  │
                    └───────┬──────────────────┼─────────────────────┼───────────┘
                            │                  │                     │
              HTTP reverse  │                  │ gRPC mTLS           │ gRPC mTLS
              proxy         │                  │ (Registration)      │ (SandboxControl)
                            │                  │ register runner     │ create / delete sandbox
               ┌────────────▼──────────────────┼─────────────────────┼──────────────┐
               │  Runner (DinD container)      │                     │              │
               │                               │                     │              │
               │  ┌────────────────────────────┼─────────────────────┼───────────┐  │
               │  │ Runner (Go app)            │                     │           │  │
               │  │                            │                     │           │  │
               │  │  ┌────────────────┐ ┌───────────────────┐ ┌──────▼────────┐  │  │
               │  │  │ Sandbox HTTP   │ │ Registration      │ │ Control       │  │  │
               │  │  │ Proxy (:8080)  │ │ gRPC Client       │ │               │  │  │
               │  │  │                │ │                   │ │ gRPC Server   │  │  │
               │  │  │                │ │                   │ │ (:9091)       │  │  │
               │  │  └───────┬────────┘ └───────────────────┘ └───────┬───────┘  │  │
               │  └──────────┼────────────────────────────────────────┼──────────┘  │
               │             │                                        │             │
               │  ┌──────────▼────────────────────────────────────────▼──────────┐  │
               │  │                    Container Manager                         │  │
               │  │               (Docker-in-Docker daemon)                      │  │
               │  └──────┬─────────────────┬─────────────────┬───────────────────┘  │
               │         │                 │                 │                      │
               │  ┌──────▼───────┐  ┌──────▼───────┐  ┌─────▼────────┐              │
               │  │ Sandbox      │  │ Sandbox      │  │ Sandbox      │              │
               │  │ container    │  │ container    │  │ container    │              │
               │  │ ┌──────────┐ │  │ ┌──────────┐ │  │ ┌──────────┐ │              │
               │  │ │  Daemon  │ │  │ │  Daemon  │ │  │ │  Daemon  │ │              │
               │  │ │ (:8081)  │ │  │ │ (:8081)  │ │  │ │ (:8081)  │ │              │
               │  │ └──────────┘ │  │ └──────────┘ │  │ └──────────┘ │              │
               │  └──────────────┘  └──────────────┘  └──────────────┘              │
               └────────────────────────────────────────────────────────────────────┘
```

The system has three tiers:

1. **API Gateway** — public entry point; routes requests, manages state, coordinates runners
2. **Runner** — manages sandbox container lifecycle via Docker-in-Docker; proxies exec/file operations to daemons
3. **Daemon** — lightweight HTTP server running inside each sandbox container; executes commands and manages files

Multiple runners can register with a single API gateway for horizontal scaling. The API distributes sandbox creation across runners using round-robin placement.

## Components

### API Gateway

**Source:** `cmd/api/`, `internal/api/`

The API gateway is the single public-facing service. It exposes a REST API for sandbox lifecycle management and proxies exec/file operations to the correct runner.

| Subcomponent | Location | Responsibility |
| --- | --- | --- |
| HTTP handlers | `internal/api/handlers.go` | Sandbox CRUD, reverse proxy to runners |
| Gateway setup | `internal/api/gateway.go` | Route registration, middleware chain |
| Runner registry | `internal/api/registry/` | In-memory registry of connected runners, round-robin placement |
| gRPC server | `internal/api/grpc/` | `RunnerRegistry` service — accepts runner heartbeat streams |
| Store | `internal/api/store/` | SQLite-backed sandbox metadata (ID, status, runner assignment) |
| Idle sweeper | `internal/api/ttl.go` | Periodic scan to stop/delete idle sandboxes |
| Config | `internal/api/config/` | Environment variable parsing and validation |

**Middleware chain:** Auth (API key) → Logging → CORS (optional) → Recovery

### Runner

**Source:** `cmd/runner-docker/`, `cmd/runner-firecracker.ee/`, `internal/runner/`

Each runner hosts sandboxes through the shared `runtime.Runtime` contract. The Docker runner manages containers via an inner Docker daemon (Docker-in-Docker), while the Firecracker runner manages microVM sandboxes. Runners are stateless — all persistent state lives in the API gateway's SQLite store.

| Subcomponent | Location | Responsibility |
| --- | --- | --- |
| Runtime contract | `internal/runner/runtime/` | Shared runner backend interface for Docker and Firecracker implementations |
| Docker runtime | `internal/runner/runtime/docker/` | Create, stop, delete containers; reconcile on startup; manage Docker network |
| Firecracker runtime | `internal/runner/runtime/firecracker.ee/` | Create, stop, delete microVM sandboxes; manage jailer, snapshot restore, and host networking ([snapshot portability](../firecracker-intel-snapshot-compat-report.md)) |
| Docker client | `internal/runner/runtime/docker/docker_client.go` | Thin wrapper around the `docker` CLI |
| Registration client | `internal/runner/register/` | gRPC heartbeat stream to API; sends capacity and health info every 10s |
| gRPC control server | `internal/runner/grpc_control.go` | `SandboxControl` service — accepts create/stop/delete RPCs from API |
| HTTP proxy | `internal/runner/proxy.go` | Reverse proxy from runner HTTP to sandbox daemon |
| Network rules | `internal/runner/runtime/docker/netrules/` | iptables rules for Docker sandbox network isolation |
| Resource limits | `internal/runner/runtime/docker/resource_limits.go` | Memory, CPU, PID, and disk quota enforcement |

**Middleware chain:** Auth (API key) → Logging → Recovery

### Daemon

**Source:** `cmd/daemon/`, `internal/daemon/`

A lightweight HTTP server embedded in every sandbox container. It is the only process that runs commands and touches files inside the sandbox.

| Subcomponent | Location | Responsibility |
| --- | --- | --- |
| HTTP server | `internal/daemon/daemon.go` | Route registration, request handling |
| Exec manager | `internal/daemon/exec_manager.go` | Track active and completed executions |
| Execution | `internal/daemon/exec.go`, `execution.go` | Fork processes, capture stdout/stderr, stream NDJSON events |
| File operations | `internal/daemon/files.go` | Read, write, append, copy, move, delete, list, stat |
| Protocol | `internal/daemon/protocol.go` | NDJSON event format with sequence numbers |

## Communication Patterns

### Client → API (REST + API Key)

All client requests go through the API gateway over HTTP. Authentication uses an `X-Api-Key` header validated with constant-time comparison.

### API ↔ Runner Registration (gRPC Bidirectional Streaming + mTLS)

Runners register with the API by opening a long-lived gRPC stream (`RunnerRegistry.Connect`). The runner sends periodic heartbeats containing its ID, HTTP base URL, health status, and capacity metrics. The API uses these to maintain a live registry and route requests. The gRPC channel is secured with mutual TLS and an additional bearer token.

**Proto definition:** `proto/runner/v1/runner.proto`

### API → Runner Control (gRPC Unary + mTLS)

When a client creates or deletes a sandbox, the API calls the runner's `SandboxControl` gRPC service (`CreateSandbox`, `StopSandbox`, `DeleteSandbox`). This channel also uses mTLS with API key authentication in gRPC metadata.

### API/Runner → Daemon (HTTP Reverse Proxy)

Exec and file operation requests are proxied through two hops:

```text
Client → API (HTTP) → Runner (HTTP reverse proxy) → Daemon (HTTP on :8081)
```

Each hop uses `httputil.ReverseProxy` with URL rewriting. The runner can wake a stopped container before proxying.

## Key Data Flows

### Creating a Sandbox

1. Client sends `POST /sandboxes` with API key
2. API picks a runner via round-robin from the registry
3. API calls `SandboxControl.CreateSandbox` on the selected runner (gRPC)
4. Runner creates a Docker container with the sandbox image, resource limits, and labels
5. Runner waits for the container to get a network IP and the daemon to become healthy
6. API stores the sandbox record in SQLite (ID, status, runner assignment)
7. API returns the sandbox ID and status to the client

### Executing a Command

1. Client sends `POST /sandboxes/{id}/executions` with command, env, and working directory
2. API looks up the sandbox in SQLite, proxies the request to the runner's HTTP endpoint
3. Runner proxies to the daemon at `{container_ip}:8081/executions` using a retry-aware exec proxy
4. Daemon forks the process, streams stdout/stderr as NDJSON events
5. Events stream back through the proxy chain to the client. If the runner→daemon connection drops mid-stream, the runner automatically resumes via `GET /executions/{exec_id}?follow=true&after=<seq>` (up to 3 retries)
6. Client can poll `GET /sandboxes/{id}/executions/{exec_id}` or cancel with `DELETE`

### File Operations

File read, write, list, stat, copy, move, and delete follow the same two-hop reverse proxy path. The daemon performs all file system operations inside the container. Request body size is capped (default 10 MB).

## Security Model

| Layer | Mechanism | Purpose |
| --- | --- | --- |
| Client → API | `X-Api-Key` header (constant-time comparison) | Authenticate API consumers |
| API ↔ Runner registration | mTLS + bearer token | Authenticate runners during gRPC registration |
| API → Runner control | mTLS + API key in gRPC metadata | Authenticate control-plane RPCs |
| File paths | Path resolution and validation | Prevent directory traversal |
| Network isolation | iptables rules on runner | Block sandbox access to private IP ranges |
| Resource limits | cgroups + xfs disk quotas | Memory, CPU, PID count, disk space per sandbox |
| Request size | Configurable body size limits | Prevent oversized uploads |
| Error sanitization | Strip internal paths from responses | Avoid leaking server internals |

TLS certificates can be bootstrapped locally with `scripts/bootstrap-mtls.sh` or managed in Kubernetes with cert-manager (see [cert-manager-k8s.md](cert-manager-k8s.md)).

## Data Storage

### API Gateway SQLite

The API persists sandbox metadata in a SQLite database at `/var/lib/n8n-sandbox-api/api.db`. The schema tracks sandbox ID, status, timestamps, container IP, daemon port, and runner assignment. Migrations run automatically on startup.

### Runner Stateless

Runners hold no persistent state. Container information is retrieved from the Docker daemon. On startup, the runner reconciles its containers (cleans up orphans, rebuilds its in-memory map).

### Daemon In-Memory

Execution results are held in memory as circular event buffers (default max 16 MiB per execution, retained for 10 minutes). No disk persistence.
