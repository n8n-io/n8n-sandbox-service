# Sandbox Service Security FAQ (Draft)

This document is intentionally Q/A only.  
For concrete gaps and the prioritized hardening roadmap, see `docs/security-weak-points-and-hardening.md`.

## FAQ

### 1) Is control-plane abuse only possible with shared runner certs?

No. Shared certs increase blast radius, but abuse is also possible via:

- Steal any valid runner client key+cert trusted by the API CA.
- Steal the registration bearer token.
- Compromise an already-registered runner process and send forged heartbeats.

### 2) Shared runner certs vs per-runner certs: what changes for `runner_id` correlation?

- Shared cert/key across runners: cert identity is the same, so cert-based runner distinction is weak.
- Per-runner cert/key: each runner has unique identity (SAN/CN/SPIFFE), so API can enforce `runner_id -> cert identity`.

Without identity binding checks, even per-runner certs do not fully prevent spoofing via other stolen credentials.

### 3) Shared runner certs vs per-runner certs: what changes for revocation enforcement?

- Shared cert/key model: revocation affects all runners sharing that identity.
- Per-runner cert/key model: you can revoke one runner identity without rotating all runners.

Current implementation validates certificate chains against trusted CAs, but does not enforce explicit CRL/OCSP checks in these code paths.

### 4) If someone has a shared cert, can they listen to other runners' traffic?

Usually not by certificate possession alone.

mTLS cert possession allows endpoint authentication, not passive decryption by itself. Decryption or impersonation still requires network position (MITM/on-path) or endpoint compromise.

- Only cert/key stolen, no network path: cannot passively decrypt unrelated traffic.
- Compromised runner host or overlay access: traffic sniffing/routing may become possible.
- MITM + trusted CA/name path: attacker can impersonate endpoints.

### 5) How would we notice a runner takeover, and what should we do?

Likely signals:

- Unexpected runner registration churn (new IDs, frequent reconnects, duplicate IDs).
- Sudden `control_grpc_addr` or `http_base_url` changes for an existing runner.
- Unusual sandbox creation/deletion rate or capacity usage spikes.
- Runner making network calls to forbidden destinations.
- Daemon exec/file activity patterns outside normal workload.
- TLS/auth failures increasing (bad token, bad cert, hostname mismatch).

Immediate response:

1. Quarantine runner node/container from network.
2. Stop scheduling new sandboxes to that runner.
3. Rotate registration token, API keys, and mTLS certs/keys.
4. Revoke compromised runner identity (CA trust or allowlist entry).
5. Rebuild runner from trusted image; do not "clean in place".
6. Investigate API/other runners for lateral movement.
7. Backfill timeline from logs and store records; notify incident stakeholders.

### 6) What happens if someone escapes from a sandbox to the runner?

Typical worst-case outcomes:

- Runner takeover and tampering with lifecycle/control logic.
- Cross-sandbox compromise on the same runner.
- Control-plane abuse (spoofed heartbeats, rogue placement, fake runner identity).
- Secret theft (tokens, certs, API keys, environment secrets, mounted files).
- Lateral movement to host/network/cloud resources.
- Persistence via modified images/scripts/startup paths.
- Data integrity loss (tampered outputs, deleted/corrupted state).
- Availability impact (resource exhaustion, service interruption).

Treat sandbox escape as runner compromise.
