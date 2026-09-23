# Architecture

The n8n Sandbox Service provides isolated, on-demand execution environments via a REST API. Each sandbox is a Debian-based container (Docker/Sysbox runner) or microVM (Firecracker runner) with an HTTP daemon inside that handles command execution and file operations. The service is designed for horizontal scalability: runners register dynamically with a central API gateway, which routes client requests to the appropriate runner and sandbox.

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

The diagram shows the Docker runner. The Firecracker runner replaces the Docker-in-Docker daemon and containers with one jailer-launched microVM per sandbox; the store is SQLite or Postgres.

The system has three tiers:

1. **API Gateway** — public entry point; routes requests, manages state, coordinates runners
2. **Runner** — manages sandbox lifecycle (containers or microVMs); proxies exec/file operations to daemons
3. **Daemon** — lightweight HTTP server running inside each sandbox; executes commands and manages files

Multiple runners can register with a single API gateway for horizontal scaling. The API distributes sandbox creation across eligible runners using load-aware placement (lowest `capacity_used`).

### Multi-pod API (Postgres)

For multiple API replicas (e.g. n8n Cloud), set `SANDBOX_API_STORE=postgres`. All pods share:

- **Sandbox metadata** (`sandboxes` table) — any pod can proxy, get, or delete any sandbox using stored runner routing info
- **Runner registry** (`runners` table) — heartbeats from gRPC streams on any pod are visible cluster-wide
- **Sweeper leadership** — a Postgres session advisory lock ensures only one pod runs the idle stop/delete sweeper per tick

SQLite remains the default for single-pod deployments (`SANDBOX_API_STORE=sqlite` or unset).

## Components

### API Gateway

**Source:** `cmd/api/`, `internal/api/`

The API gateway is the single public-facing service. It exposes a REST API for sandbox lifecycle management and proxies exec/file operations to the correct runner.

| Subcomponent | Location | Responsibility |
| --- | --- | --- |
| HTTP handlers | `internal/api/handlers.go` | Sandbox CRUD, reverse proxy to runners |
| Gateway setup | `internal/api/gateway.go` | Route registration, middleware chain |
| Runner registry | `internal/api/registry/` | Runner heartbeats and placement (in-memory for SQLite; Postgres for multi-pod) |
| gRPC server | `internal/api/grpc/` | `RunnerRegistry` service — accepts runner heartbeat streams |
| Store | `internal/api/store/` | Sandbox metadata (`sqlite` default, `postgres` for multi-pod) |
| Sweeper lock | `internal/api/store/postgres.go` | Postgres advisory lock for idle sweeper leadership |
| Idle sweeper | `internal/api/ttl.go` | Periodic scan to stop/delete idle sandboxes (ephemeral ones are deleted, never stopped) |
| Config | `internal/api/config/` | Environment variable parsing and validation |

**Middleware chain:** Recovery → CORS (optional) → Logging → Auth (API key) → Metrics (optional)

### Runner

**Source:** `cmd/runner-docker/`, `cmd/runner-firecracker.ee/`, `internal/runner/`

Each runner hosts sandboxes through the shared `runtime.Runtime` contract. The Docker runner manages containers via an inner Docker daemon (Docker-in-Docker), while the Firecracker runner manages microVM sandboxes. Runners are stateless — persistent sandbox metadata lives in the API store (SQLite or Postgres).

| Subcomponent | Location | Responsibility |
| --- | --- | --- |
| Runtime contract | `internal/runner/runtime/` | Shared runner backend interface for Docker and Firecracker implementations |
| Docker runtime | `internal/runner/runtime/docker/` | Create, stop, delete containers; reconcile on startup; manage Docker network |
| Firecracker runtime | `internal/runner/runtime/firecracker.ee/` | Create, stop, delete microVM sandboxes; manage jailer, snapshot restore, and host networking |
| Docker client | `internal/runner/runtime/docker/docker_client.go` | Thin wrapper around the `docker` CLI |
| Registration client | `internal/runner/register/` | gRPC heartbeat stream to API; sends capacity and health info every 10s |
| gRPC control server | `internal/runner/grpc_control.go` | `SandboxControl` service — accepts create/stop/delete RPCs from API |
| HTTP proxy | `internal/runner/proxy.go` | Reverse proxy from runner HTTP to sandbox daemon |
| Network rules | `internal/runner/runtime/docker/netrules/` | iptables rules for Docker sandbox network isolation |
| Resource limits | `internal/runner/runtime/docker/resource_limits.go` | Memory, CPU, PID, and disk quota enforcement |

**Middleware chain:** Recovery → Logging → Auth (client certificate + API key) → Metrics (optional)

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

All client requests go through the API gateway over HTTP. Authentication uses an `X-Api-Key` header. Keys in `SANDBOX_API_KEYS` are admin keys (full access). Keys in `SANDBOX_API_PROVISIONER_KEYS` are provisioner keys (see [security-model.md](security-model.md#provisioner-keys)). Admin-minted tenant keys (stored hashed in the API database) are scoped to that tenant's sandboxes.

Per-tenant `max_sandboxes` is enforced with a soft check-then-act before create. Concurrent creates can exceed the quota by up to the number in flight and burn shared runner capacity; the limit is not an atomic reservation.

### API ↔ Runner Registration (gRPC Bidirectional Streaming + mTLS)

Runners register with the API by opening a long-lived gRPC stream (`RunnerRegistry.Connect`). The runner sends periodic heartbeats containing its ID, HTTP base URL, health status, and capacity metrics. The API uses these to maintain a live registry and route requests. The gRPC channel is secured with mutual TLS and an additional bearer token.

**Proto definition:** `proto/runner/v1/runner.proto`

### API → Runner Control (gRPC Unary + mTLS)

When a client creates or deletes a sandbox, the API calls the runner's `SandboxControl` gRPC service (`CreateSandbox`, `StopSandbox`, `DeleteSandbox`). This channel also uses mTLS with API key authentication in gRPC metadata.

The runner's HTTP listener uses the same certificate and the same client CA, so both channels to a runner share one identity. It negotiates with `VerifyClientCertIfGiven` so that health probes, which cannot present a certificate, still reach `/livez` and `/readyz`; the auth middleware requires a verified certificate on every other route.

### API/Runner → Daemon (HTTP Reverse Proxy)

Exec and file operation requests are proxied through two hops:

```text
Client → API (HTTP) → Runner (HTTP reverse proxy) → Daemon (HTTP on :8081)
```

Each hop uses `httputil.ReverseProxy` with URL rewriting. The runner can wake a stopped container before proxying.

## Key Data Flows

### Creating a Sandbox

1. Client sends `POST /sandboxes` with API key
2. API picks the eligible runner with the lowest reported `capacity_used`
3. API calls `SandboxControl.CreateSandbox` on the selected runner (gRPC)
4. Runner creates the sandbox: a container from the sandbox image with resource limits and labels, or a microVM restored from the golden snapshot
5. Runner waits for the daemon inside it to become healthy
6. API stores the sandbox record (ID, tenant, status, runner assignment)
7. API returns the sandbox ID and status to the client

### Executing a Command

1. Client sends `POST /sandboxes/{id}/executions` with command, env, and working directory
2. API looks up the sandbox in the store, proxies the request to the runner's HTTP endpoint
3. Runner proxies to the sandbox daemon's `/executions` (container IP on Docker, a host-local per-slot port on Firecracker) using a retry-aware exec proxy
4. Daemon forks the process, streams stdout/stderr as NDJSON events
5. Events stream back through the proxy chain to the client. If the runner→daemon connection drops mid-stream, the runner automatically resumes via `GET /executions/{exec_id}?follow=true&after=<seq>` (up to 3 retries)
6. Client can poll `GET /sandboxes/{id}/executions/{exec_id}` or cancel with `DELETE`

### File Operations

File read, write, list, stat, copy, move, and delete follow the same two-hop reverse proxy path. The daemon performs all file system operations inside the container. Request body size is capped (default 10 MB).

### Recovering a Crashed Guest

A sandbox can lose its guest (daemon exit, kernel panic, OOM) without losing its files. Both backends bring it back on the next request and answer that request with `409 sandbox_restarted` instead of proxying it, because memory — running processes, execution history — is gone; the retry succeeds ([API.md](API.md#http-409-sandbox_restarted--the-sandbox-came-back-without-its-memory)). Each recovery increments `sandbox_recoveries_total`.

**Firecracker.** The guest daemon is PID 1 with `panic=1 reboot=k`, and jailer execs Firecracker in place, so the process the runner waits on exits when the guest dies. A generation counter bumped before every deliberate kill tells that apart from a runner-initiated stop. The dead sandbox is torn down and releases its slot immediately (stopped, files intact, no usable snapshot), so an untouched crashed sandbox costs only disk. The next request wakes it by cold booting its own rootfs with the boot parameters recorded at create. Its stale snapshot is never restored: a memory image whose cached filesystem metadata predates later disk writes would corrupt the rootfs silently. A sandbox is pinned to cold boot from the moment its snapshot is restored, and only a clean stop (pause, then snapshot) lifts the pin. Cold boot keeps the files and skips the rootfs/snapshot clones that dominate a create.

**Docker.** Containers run with `--restart unless-stopped`, so Docker restarts the container itself. The runner learns of the death from a `docker events` stream filtered to `die`; deliberate stops and removes are recorded before the call and matched against events, and exit codes are ignored (an exit `0` still lost everything running). The sandbox is reported not running until the wake path reapplies network policy to the possibly new container IP, waits for the daemon, and reports the restart. If the event stream drops, the watcher reconnects; a death missed meanwhile is served without its `409`.

The report is spent by the first request that wakes the sandbox, so `DELETE /sandboxes/{id}/executions/{exec_id}` — which the SDK sends in the background after every command — returns `204` without waking a stopped sandbox, leaving the `409` for the next request a client reads.

## Security Model

Trust boundaries, the mechanism enforcing each, and the non-guarantees are in [security-model.md](security-model.md). Certificates are bootstrapped locally with `scripts/bootstrap-mtls.sh` or managed in Kubernetes with cert-manager ([cert-manager-k8s.md](cert-manager-k8s.md)).

## Data Storage

### API store

SQLite at `SANDBOX_API_DATA_DIR/api.db` (default) or Postgres. Tables: `sandboxes` (ID, tenant, status, timestamps, runner routing), `tenants`, `api_keys` (hashed), and on Postgres `runners` (heartbeats; SQLite keeps the runner registry in memory). Migrations run automatically on startup.

### Runner stateless

Runners hold no persistent state. On startup each runtime removes every sandbox left over from a previous run and starts empty; sandboxes do not survive a runner restart.

### Daemon In-Memory

Execution results are held in memory as circular event buffers (default max 16 MiB per execution, retained for 10 minutes). No disk persistence.
