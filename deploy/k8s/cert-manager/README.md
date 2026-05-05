# Kubernetes: runner registration gRPC with cert-manager (mTLS)

Use a private CA (cert-manager `ClusterIssuer` of type CA, or another issuer you operate) that signs leaf certificates for TLS roles:

1. **API** — server cert with DNS SAN matching the Service DNS runners use (for example `sandbox-api.sandbox.svc.cluster.local`).
2. **Runner** — client cert (`clientAuth` EKU) presented when runners dial the API.

Mount PEMs from `Certificate` secrets (typically `tls.crt`, `tls.key`, and a CA bundle) and set:

- **API:** `SANDBOX_API_GRPC_TLS_CERT_FILE`, `SANDBOX_API_GRPC_TLS_KEY_FILE`, `SANDBOX_API_GRPC_TLS_CLIENT_CA_FILE` (PEM of the CA that signs runner clients).
- **Runner:** `SANDBOX_RUNNER_GRPC_TLS_CA_FILE` (PEM of the CA that signed the API server cert), `SANDBOX_RUNNER_GRPC_TLS_CERT_FILE`, `SANDBOX_RUNNER_GRPC_TLS_KEY_FILE`, `SANDBOX_RUNNER_GRPC_TLS_SERVER_NAME` (must match a DNS SAN on the API server cert).

## cert-manager sketch

- One CA `Issuer` (or a CA you already run).
- `Certificate` for the API gRPC server — `usages: [ "server auth" ]`.
- `Certificate` for runners as TLS clients — `usages: [ "client auth" ]`, per-runner.

Mount each secret’s files into the API and runner Pods at the paths referenced by the env vars above.

When `cert-manager` renews a leaf, updated files appear in the volume; the service binaries reload leaf key pairs from disk on the next TLS handshake. Rotating a CA that peers trust usually means updating mounted CA PEMs and rolling workloads. Prefer short-lived leaves and keep CA rotation infrequent.
