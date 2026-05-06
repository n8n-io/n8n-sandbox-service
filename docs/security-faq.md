# Sandbox Service Security FAQ (Draft)

This document captures threat-model notes for the current sandbox service architecture and a practical hardening plan.

Scope:

- API service (`cmd/api`, `internal/api/*`)
- Runner service (`cmd/runner`, `internal/runner/*`)
- In-sandbox daemon (`internal/daemon/*`)

## FAQ

### 1) Is control-plane abuse only possible with shared runner certs?

No. Shared certificates make impersonation easier, but they are not the only path.

Control-plane abuse is possible when an attacker can act as a trusted runner identity in any of these ways:

- Steal any valid runner client key+cert that chains to the trusted runner CA.
- Steal the registration bearer token.
- Compromise an already-registered runner process and send forged heartbeats.

What shared runner certs change:

- **With shared cert/key across runners:** compromise of one runner can impersonate all runners immediately.
- **With per-runner certs:** blast radius is smaller, but abuse is still possible unless API enforces runner identity binding.

Identity binding that should be enforced:

- Map certificate identity (SPIFFE URI or cert SAN/CN) to one allowed `runner_id`.
- Reject heartbeats where `runner_id` does not match the authenticated cert identity.
- Optionally pin expected `control_grpc_addr`/`http_base_url` per runner.

### 2) If someone has a shared cert, can they listen to other runners' traffic?

Usually not by certificate possession alone.

mTLS cert possession allows authentication as a client/server, but passive decryption still requires network position (MITM/on-path), endpoint compromise, or traffic capture with session key access.

Practical cases:

- **Only cert/key stolen, no network path:** cannot passively decrypt unrelated runner traffic.
- **Compromised runner host or overlay network access:** may sniff/route traffic depending on network controls.
- **Can perform MITM and clients trust same CA/name:** can impersonate endpoints and terminate TLS.

So routing/network segmentation still matters a lot; cert compromise plus network control is the dangerous combination.

### 3) How would we notice a runner takeover, and what should we do?

Likely signals:

- Unexpected runner registration churn (new IDs, frequent reconnects, duplicate IDs).
- Sudden `control_grpc_addr` or `http_base_url` changes for an existing runner.
- Unusual sandbox creation/deletion rate or capacity usage spikes.
- Runner making network calls to forbidden destinations.
- Daemon exec/file activity patterns outside normal workload.
- TLS/auth failures increasing (bad token, bad cert, hostname mismatch).

Immediate response (treat as critical):

1. Quarantine runner node/container from network.
2. Stop scheduling new sandboxes to that runner.
3. Rotate registration token, API keys, and mTLS certs/keys.
4. Revoke compromised runner identity (CA trust or allowlist entry).
5. Rebuild runner from trusted image; do not "clean in place".
6. Investigate API/other runners for lateral movement.
7. Backfill timeline from logs and store records; notify incident stakeholders.

## What happens on sandbox escape to runner

Worst-case outcomes:

- Runner takeover and tampering with lifecycle/control logic.
- Cross-sandbox compromise on the same runner.
- Control-plane abuse (spoofed heartbeats, rogue placement, fake runner identity).
- Secret theft (tokens, certs, API keys, environment secrets, mounted files).
- Lateral movement to host/network/cloud resources.
- Persistence via modified images/scripts/startup paths.
- Data integrity loss (tampered outputs, deleted/corrupted state).
- Availability impact (resource exhaustion, service interruption).

Operationally, sandbox escape should be handled as "runner fully compromised."

## Current weak points (specific to this repo)

### API

- **Registration auth is token+CA based, not runner-identity bound.**
  - `internal/api/grpc/runner_server.go` validates bearer token and accepts any cert signed by configured CA, but does not bind cert identity to `runner_id`.

- **Registration accepts runner-provided routing fields with limited constraints.**
  - `http_base_url` is URL-validated, but `control_grpc_addr` is only checked for non-empty (not strict `host:port` or allowlist) in `internal/api/grpc/runner_server.go`.

- **CORS is fully open while API-key auth is header-based.**
  - `internal/api/middleware.go` sets `Access-Control-Allow-Origin: *` and allows `X-Api-Key`; this can broaden exposure in browser-facing deployments unless tightly fronted.

### Runner

- **Same shared API key used across runner HTTP and control gRPC paths.**
  - `internal/runner/middleware.go` and `internal/runner/grpc_control.go` both rely on static API key list; no per-caller identity separation.

- **Runner HTTP control plane is authenticated but not mTLS-protected.**
  - HTTP endpoints are API-key based (`internal/runner/server.go` + middleware), so key leakage is high impact on that path.

- **Container trust boundary depends heavily on host/Docker isolation.**
  - `internal/runner/manager/manager.go` manages Docker directly; escape assumptions are delegated to container runtime/kernel hardening.

- **Sandbox containers are not strongly hardened at runtime.**
  - Container creation sets `--user` and resource limits, but does not add stronger controls such as capability drops, seccomp/apparmor policy pinning, readonly rootfs, or `no-new-privileges` in `internal/runner/manager/manager.go`.

- **Sandbox image includes high-risk tooling and passwordless sudo.**
  - `Dockerfile.sandbox` installs many tools and grants `NOPASSWD:ALL` sudo, which increases post-exploit capability if the sandbox is compromised.

### Daemon (inside sandbox)

- **Daemon has broad command/file capability by design.**
  - `/exec` runs `/bin/sh -c` (`internal/daemon/exec.go`), and file operations support read/write/copy/move/delete (`internal/daemon/daemon.go`).
  - This is expected for sandbox functionality, but it means daemon compromise equals full sandbox compromise.

- **Daemon transport is plaintext on internal container network.**
  - Runner talks to daemon over `http://<container-ip>:8081` (`internal/runner/manager/manager.go` and `internal/runner/proxy.go`), relying on network isolation rules rather than TLS/auth at daemon layer.

- **No daemon-level auth because trust is network-based.**
  - If network isolation/routing is bypassed, daemon endpoints are directly abusable.

- **Network policy implementation is IPv4-focused.**
  - `internal/runner/netrules/netrules.go` enforces private-range blocking via iptables IPv4 rules; equivalent IPv6 policy controls are not present in this path.

## Prioritized hardening plan

### P0 (do first)

- **Enforce runner identity binding on registration.**
  - Extract peer cert identity from gRPC context.
  - Maintain allowlist: `runner_id -> expected cert identity`.
  - Reject mismatches.
  - Alert when the same `runner_id` appears with a different cert identity.

- **Move to per-runner client certificates (no shared private keys).**
  - Unique keypair per runner.
  - Short cert TTLs, automated rotation.

- **Validate and constrain runner-advertised addresses.**
  - Enforce strict `host:port` for `control_grpc_addr`.
  - Optionally require host to match runner identity or pre-approved domain suffix.

- **Add takeover detection alerts.**
  - Alert on `runner_id` reuse from new cert identity.
  - Alert on address flips, heartbeat anomalies, sandbox lifecycle spikes.

- **Harden sandbox runtime defaults.**
  - Add capability drops and `no-new-privileges`.
  - Pin seccomp/apparmor (or equivalent) policy.
  - Prefer readonly rootfs with explicit writable mounts.

### P1 (next)

- **Add revocation/rapid invalidation strategy.**
  - Prefer short-lived certs + frequent rotation.
  - Add denylist/allowlist checks on cert serial/SPIFFE IDs.

- **Split credentials by purpose.**
  - Separate API keys for runner HTTP proxy vs control gRPC calls.
  - Consider removing API-key dependency from gRPC and relying on mTLS identity + authorization policy.

- **Reduce runner blast radius on compromise.**
  - Tighten node-level egress ACLs.
  - Restrict access to cloud metadata and internal control-plane networks.
  - Lock down secrets mounted into runner containers.

- **Close network policy gaps (IPv4 + IPv6).**
  - Ensure sandbox egress controls cover IPv6 and are validated in tests.
  - Verify behavior across iptables/nftables variants in runner environments.

### P2 (defense in depth)

- **Strengthen daemon boundary.**
  - Add daemon auth token scoped per sandbox session.
  - Optionally use mTLS or unix-socket transport where feasible.

- **Improve forensic readiness.**
  - Structured security audit events for registration/control actions.
  - Immutable log shipping and retention.

- **Add security regression tests.**
  - Tests for cert identity mismatch, stale cert rejection, rogue runner registration, invalid advertised addresses, and control-plane abuse attempts.

## Operational notes (non-security-primary)

- API shutdown currently uses forceful gRPC stop for registration streams (`cmd/api/main.go`, `grpcSrv.Stop()`), which is mostly an availability/operability trade-off rather than a direct security weakness.

## Notes on certificate strategy

- Sharing only the CA public cert is normal and expected.
- Sharing runner private keys is not acceptable for production.
- The API should trust a CA bundle but still apply runner-specific authorization based on authenticated cert identity.
- Current code path verifies cert chains via trusted CA but does not implement explicit CRL/OCSP revocation checks; prefer short-lived certs and rapid rotation/denylisting.

---

Status: draft. Update this FAQ as controls are implemented so it stays operationally accurate.
