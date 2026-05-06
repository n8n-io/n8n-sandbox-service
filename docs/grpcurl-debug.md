# Debugging gRPC with grpcurl

[`grpcurl`](https://github.com/fullstorydev/grpcurl) invokes gRPC methods from the command line. This repo keeps protos under `proto/runner/v1/runner.proto`.

Install (examples):

- macOS: `brew install grpcurl`
- Or download a release binary from the project’s GitHub releases page.

From the **repository root**, pass import path and proto file:

```bash
PROTO_IMPORT=.
PROTO_FILE=proto/runner/v1/runner.proto
```

## Runner: `SandboxControl` (unary)

The runner listens on `SANDBOX_RUNNER_CONTROL_GRPC_LISTEN_ADDR` (compose default advertise addresses: `n8n-sandbox-runner-local-1:9091` and `n8n-sandbox-runner-local-2:9091` on the Docker `sandbox` network).

```bash
TLS_DIR=.tls
grpcurl \
  -import-path "$PROTO_IMPORT" -proto "$PROTO_FILE" \
  -cacert "$TLS_DIR/ca.crt" \
  -cert "$TLS_DIR/control-grpc-api-client.crt" \
  -key "$TLS_DIR/control-grpc-api-client.key" \
  -H "x-api-key: ${RUNNER_API_KEY:-runner-local-key}" \
  -d "{\"sandbox_id\":\"$(uuidgen | tr '[:upper:]' '[:lower:]')\",\"create_json\":\"{}\"}" \
  n8n-sandbox-runner-local-1:9091 \
  runner.v1.SandboxControl/CreateSandbox
```

The bootstrap script puts **both** compose runner hostnames in the **same** control server certificate SAN list, so one `control-grpc-server` cert works on every local runner.

## API: `RunnerRegistry` (bidirectional stream)

Registration uses a **client-streaming / server-streaming** RPC (`Connect`), not a single unary call. `grpcurl` can be awkward here; prefer exercising registration by running a runner process or a small Go test client.

For a quick **TLS sanity check** that the API registry port answers with TLS (and requires client certificates), you can use `openssl` instead of `grpcurl`:

```bash
openssl s_client -connect localhost:9090 -servername n8n-sandbox-api-local -brief </dev/null
```

Adjust host, port, and `-servername` to match your deployment and the DNS SAN on the API registration server certificate.

## Listing services

With protos on disk:

```bash
grpcurl -import-path "$PROTO_IMPORT" -proto "$PROTO_FILE" list n8n-sandbox-runner-local-1:9091
```

If the server has [gRPC reflection](https://grpc.io/docs/guides/reflection/) enabled (this service does not enable it by default), you can omit `-import-path` / `-proto` and use `grpcurl list HOST:PORT`.
