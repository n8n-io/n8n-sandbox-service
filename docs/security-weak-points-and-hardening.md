# Sandbox Security Weak Points and Hardening Plan (Draft)

Scope:

- API service (`cmd/api`, `internal/api/*`)
- Runner service (`cmd/runner`, `internal/runner/*`)
- In-sandbox daemon (`internal/daemon/*`)

## Current weak points (specific to this repo)

### API

- Registration auth is token+CA based, not runner-identity bound.
  - `internal/api/grpc/runner_server.go` validates bearer token and accepts any cert signed by configured CA, but does not bind cert identity to `runner_id`.

- Registration accepts runner-provided routing fields with limited constraints.
  - `http_base_url` is URL-validated, but `control_grpc_addr` is only checked for non-empty (not strict `host:port` or allowlist) in `internal/api/grpc/runner_server.go`.

- CORS is fully open while API-key auth is header-based.
  - `internal/api/middleware.go` sets `Access-Control-Allow-Origin: *` and allows `X-Api-Key`; this can broaden exposure in browser-facing deployments unless tightly fronted.

### Runner

- Same shared API key used across runner HTTP and control gRPC paths.
  - `internal/runner/middleware.go` and `internal/runner/grpc_control.go` both rely on static API key list; no per-caller identity separation.

- Runner HTTP control plane is authenticated but not mTLS-protected.
  - HTTP endpoints are API-key based (`internal/runner/server.go` + middleware), so key leakage is high impact on that path.

- Container trust boundary depends heavily on host/Docker isolation.
  - `internal/runner/manager/manager.go` manages Docker directly; escape assumptions are delegated to container runtime/kernel hardening.

- Sandbox containers are not strongly hardened at runtime.
  - Container creation sets `--user` and resource limits, but does not add stronger controls such as capability drops, seccomp/apparmor policy pinning, readonly rootfs, or `no-new-privileges` in `internal/runner/manager/manager.go`.

- Sandbox image includes high-risk tooling and passwordless sudo.
  - `Dockerfile.sandbox` installs many tools and grants `NOPASSWD:ALL` sudo, which increases post-exploit capability if the sandbox is compromised.

### Daemon (inside sandbox)

- Daemon has broad command/file capability by design.
  - `/exec` runs `/bin/sh -c` (`internal/daemon/exec.go`), and file operations support read/write/copy/move/delete (`internal/daemon/daemon.go`).
  - This is expected for sandbox functionality, but it means daemon compromise equals full sandbox compromise.

- Daemon transport is plaintext on internal container network.
  - Runner talks to daemon over `http://<container-ip>:8081` (`internal/runner/manager/manager.go` and `internal/runner/proxy.go`), relying on network isolation rules rather than TLS/auth at daemon layer.

- No daemon-level auth because trust is network-based.
  - If network isolation/routing is bypassed, daemon endpoints are directly abusable.

- Network policy implementation is IPv4-focused.
  - `internal/runner/netrules/netrules.go` enforces private-range blocking via iptables IPv4 rules; equivalent IPv6 policy controls are not present in this path.

## Prioritized hardening plan

### P0 (do first)

- Enforce runner identity binding on registration.
  - Extract peer cert identity from gRPC context.
  - Maintain allowlist: `runner_id -> expected cert identity`.
  - Reject mismatches.
  - Alert when the same `runner_id` appears with a different cert identity.

- Move to per-runner client certificates (no shared private keys).
  - Unique keypair per runner.
  - Short cert TTLs, automated rotation.

- Validate and constrain runner-advertised addresses.
  - Enforce strict `host:port` for `control_grpc_addr`.
  - Optionally require host to match runner identity or pre-approved domain suffix.

- Add takeover detection alerts.
  - Alert on `runner_id` reuse from new cert identity.
  - Alert on address flips, heartbeat anomalies, sandbox lifecycle spikes.

- Harden sandbox runtime defaults.
  - Add capability drops and `no-new-privileges`.
  - Pin seccomp/apparmor (or equivalent) policy.
  - Prefer readonly rootfs with explicit writable mounts.

### P1 (next)

- Add revocation/rapid invalidation strategy.
  - Prefer short-lived certs + frequent rotation.
  - Add denylist/allowlist checks on cert serial/SPIFFE IDs.

- Split credentials by purpose.
  - Separate API keys for runner HTTP proxy vs control gRPC calls.
  - Consider removing API-key dependency from gRPC and relying on mTLS identity + authorization policy.

- Reduce runner blast radius on compromise.
  - Tighten node-level egress ACLs.
  - Restrict access to cloud metadata and internal control-plane networks.
  - Lock down secrets mounted into runner containers.

- Close network policy gaps (IPv4 + IPv6).
  - Ensure sandbox egress controls cover IPv6 and are validated in tests.
  - Verify behavior across iptables/nftables variants in runner environments.

### P2 (defense in depth)

- Strengthen daemon boundary.
  - Add daemon auth token scoped per sandbox session.
  - Optionally use mTLS or unix-socket transport where feasible.

- Improve forensic readiness.
  - Structured security audit events for registration/control actions.
  - Immutable log shipping and retention.

- Add security regression tests.
  - Tests for cert identity mismatch, stale cert rejection, rogue runner registration, invalid advertised addresses, and control-plane abuse attempts.

## Operational notes (non-security-primary)

- API shutdown currently uses forceful gRPC stop for registration streams (`cmd/api/main.go`, `grpcSrv.Stop()`), which is mostly an availability/operability trade-off rather than a direct security weakness.
