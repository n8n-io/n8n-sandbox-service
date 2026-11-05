# Volume store helper

With `SANDBOX_API_VOLUME_STORE=remote` the API keeps volume objects through a helper: a process next to the API, such as a sidecar container, that holds the storage credentials. It serves the `VolumeStore` gRPC service on the Unix socket in `SANDBOX_API_VOLUME_REMOTE_SOCKET`. [`proto/volumestore/v1/volumestore.proto`](../proto/volumestore/v1/volumestore.proto) and its comments are the contract.

- The connection has no TLS and the API sends no credentials, so only the API and the helper may reach the socket, and only the helper may write to its directory.
- The helper should also serve `grpc.health.v1.Health` on the socket, so a probe can check it without touching storage. The API does not call it, and does not connect at startup.
- The API cancels a call once nothing has been sent or received for a minute, including the wait for `Put`'s answer after the half-close, so a helper must keep reading and commit within that time.

## Conformance

`TestConformance*` in `internal/api/volumestore` checks a helper against the rules in the proto comments that need no access to its storage. It runs against the repository's fake helper, and with `SANDBOX_TEST_VOLUME_STORE_SOCKET` set, against the helper on that socket too. It stores and deletes objects with random IDs, so point the helper at a throwaway container:

```bash
SANDBOX_TEST_VOLUME_STORE_SOCKET=/path/to/helper.sock go test -count=1 -run TestConformance ./internal/api/volumestore/
```
