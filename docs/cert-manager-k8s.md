# Kubernetes: gRPC mTLS with cert-manager

The sandbox service uses two gRPC planes:

1. **Runner registration** — runners open a bidirectional stream to the API (`RunnerRegistry`). Runners present a **client** certificate; the API presents a **server** certificate and verifies clients against a CA.
2. **Sandbox control** — the API calls each runner’s `SandboxControl` service for **create/delete** sandbox lifecycle. The runner hosts the gRPC server; the API is the **client**. Proxied traffic (exec, files, etc.) stays on HTTP.

Use one private CA (for example a cert-manager `ClusterIssuer` of type CA) that signs distinct leaf roles:

| Role | Typical cert-manager `usages` | Who mounts it |
|------|------------------------------|---------------|
| API registration listener | `server auth` | API Pod |
| Runner registration client | `client auth` | Runner Pods |
| Runner SandboxControl listener | `server auth` | Runner Pods |
| API SandboxControl client | `client auth` | API Pod |

Mount PEMs from `Certificate` secrets (often `tls.crt`, `tls.key`) plus a CA bundle where needed, and set:

**API**

- Registration listener (existing): `SANDBOX_API_GRPC_TLS_CERT_FILE`, `SANDBOX_API_GRPC_TLS_KEY_FILE`, `SANDBOX_API_GRPC_TLS_CLIENT_CA_FILE` (PEM of the CA that signs **runner registration clients**).
- Control client (dialing runners): `SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CA_FILE` (CA that signed **runner control server** certs), `SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CERT_FILE`, `SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_KEY_FILE`. Optional `SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_SERVER_NAME` if the TLS server name you verify must differ from the dial host (defaults to the host portion of the runner `host:port`).

**Runner**

- Registration client (existing): `SANDBOX_RUNNER_REGISTRATION_GRPC_CA_FILE`, `SANDBOX_RUNNER_REGISTRATION_GRPC_CERT_FILE`, `SANDBOX_RUNNER_REGISTRATION_GRPC_KEY_FILE`, optional `SANDBOX_RUNNER_REGISTRATION_GRPC_SERVER_NAME` (must match a DNS SAN on the API registration server cert).
- Control listener: `SANDBOX_RUNNER_CONTROL_GRPC_TLS_CERT_FILE`, `SANDBOX_RUNNER_CONTROL_GRPC_TLS_KEY_FILE`, `SANDBOX_RUNNER_CONTROL_GRPC_TLS_CLIENT_CA_FILE` (CA that signed **API control clients**).

Also set `SANDBOX_RUNNER_CONTROL_GRPC_LISTEN_ADDR` (for example `:9091`) and either `SANDBOX_RUNNER_CONTROL_GRPC_ADVERTISE_ADDR` or a usable `SANDBOX_RUNNER_HTTP_BASE_URL` so the runner can advertise `control_grpc_addr` in heartbeats.

## cert-manager sketch

- One CA `Issuer` (or a CA you already run).
- `Certificate` for the API registration gRPC server — `usages: [ "server auth" ]`, DNS SAN matching the Service DNS runners use (for example `sandbox-api.sandbox.svc.cluster.local`).
- `Certificate` for runners as TLS clients to that API — `usages: [ "client auth" ]` (per-runner or shared, per your policy).
- `Certificate` for each runner’s SandboxControl gRPC server — `usages: [ "server auth" ]`, DNS SAN matching how the API resolves the runner (Pod DNS, headless Service, etc.).
- `Certificate` for the API as client to runners — `usages: [ "client auth" ]`.

When `cert-manager` renews a leaf, updated files appear in the volume; the service binaries reload leaf key pairs from disk on the next TLS handshake. Rotating a CA that peers trust usually means updating mounted CA PEMs and rolling workloads. Prefer short-lived leaves and keep CA rotation infrequent.
