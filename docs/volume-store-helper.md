# Volume store helper

With `SANDBOX_API_VOLUME_STORE=remote` the API keeps volume objects through a helper: a process next to the API, such as a sidecar container, that holds the storage credentials. The API speaks HTTP/1.1 to it over the Unix socket in `SANDBOX_API_VOLUME_REMOTE_SOCKET` and sends no credentials, so only the API and the helper may reach the socket, and only the helper may write to its directory.

Objects are immutable and named by the API: `shared/<id>.<kind>` or `tenants/<tenant id>/<id>.<kind>`, where `<id>` is 1 to 64 characters of `a-z`, `0-9` and `-`, `<kind>` is `ext4` or `tar`, and `<tenant id>` is a lowercase UUID. A helper should refuse any other name with `400`.

| Request | Success | Errors |
| --- | --- | --- |
| `PUT /v1/objects/{name}?max_bytes=<n>[&sha256=<hex>]` with the content as the body, usually chunked | `201`, `{"size": <n>, "sha256": "<hex>"}` | `409` the object exists, `413` more than `max_bytes`, `422` not the given `sha256` |
| `HEAD /v1/objects/{name}` | `200` with `Content-Length` and `Object-SHA256: <hex>` | `404` |
| `GET /v1/objects/{name}` | `200`, the content, with the same headers | `404` |
| `DELETE /v1/objects/{name}` | `204`, also when the object does not exist | |
| `POST /v1/read-urls`, `{"name": "<name>", "ttl_seconds": <n>}` | `200`, `{"url": "<url>"}` | |

Any other status is an error, with an optional `{"error": "<message>"}` body that the API logs, so it must not hold credentials or read URLs. Digests are lowercase hex SHA-256. The API gives up on a connection on which nothing moved for a minute.

A helper must:

- Store a `PUT` only once its body is complete, never replace an existing object, and answer `201` only for an object this `PUT` created. The API counts and hashes each upload itself and cuts it off before its end when it is too large or has the wrong digest. It deletes an object the helper reports stored before the upload ended or with another size or digest, and when an answer leaves open whether the object was stored, it asks with `HEAD`.
- Return the size and digest it recorded on `HEAD` and `GET`.
- Issue `https` read URLs without credentials in the authority that allow a plain `GET` of that one object and nothing else, and expire within `ttl_seconds` (at most 3600). The URL need not point at an existing object.
