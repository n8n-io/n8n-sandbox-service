# Sandbox Service API

All endpoints except `/healthz` and `/metrics` require the `X-Api-Key` header for authentication. `/metrics` is only exposed when `SANDBOX_API_METRICS_ENABLED=true` and is intended to be scraped over a private network.

### API keys and tenants

`SANDBOX_API_KEYS` are **admin** keys. An admin key can create/list/delete any sandbox and manage tenants (same behavior as before for self-hosted single-key setups).

Optionally, create **tenants** and mint **tenant API keys** via the admin API. A tenant key may only create sandboxes for that tenant and may only list/get/delete/proxy its own sandboxes. Self-hosted operators can ignore tenant APIs entirely and keep using the admin key.

Tenant keys are returned in plaintext **once** on create; only a hash is stored.

---

## Error Response Format

```json
{
  "error": "string",
  "code": 400
}
```

### HTTP 503 (transient) vs 502 (not retryable)

The API and runner use two buckets so clients (including the SDK) can decide **when to retry the same request** vs **when to change strategy** (e.g. create a new sandbox, fix routing, surface an error to the user).

- **503 Service Unavailable** — **Transient / retry**: overload, no capacity yet, network or upstream not ready, or the sandbox daemon is not reachable *for the moment* while the container is otherwise expected to be usable. Safe to back off and retry the same operation.
- **502 Bad Gateway** — **Not retryable as “wait and retry”**: the request does not make sense to repeat unchanged; fix state first (new sandbox, repair registry/routing, or handle the reported error). Examples: stored sandbox has **no runner HTTP base URL**, or **delete** failed on the runner control plane.

**404** `sandbox not found` — Unknown id, the sandbox is past its idle delete-after wake window (`SANDBOX_API_IDLE_DELETE_AFTER`, default `24h`), or the runner no longer tracks the sandbox (eviction, delete, or runner restart). On exec/file proxy routes, when the runner signals sandbox gone (`X-Sandbox-Gone: 1` or `{"error":"sandbox not found"}`), the API removes the store row so subsequent `GET /sandboxes/{id}` also returns 404. Other runner **404** responses (for example `execution not found` or missing file paths) do **not** delete the sandbox. Exec and file routes may return **503** or **502** from the runner after the API successfully reaches the runner; the API may return **503** `runner unavailable` before the runner is contacted.

---

## Endpoints

### GET /healthz

Health check. No authentication required.

**Response:** `200 OK`

```json
{"status": "ok"}
```

**Example:**

```bash
curl http://localhost:8080/healthz
```

---

### GET /metrics

Prometheus exposition of the API's metrics. Only mounted when `SANDBOX_API_METRICS_ENABLED=true`; bypasses `X-Api-Key` so a scraper can reach it. Firewall the listener or front it with a private LB.

Metric families include:

- `sandbox_http_requests_total{role,route,method,status}` — request counter; `route` is the matched route pattern (e.g. `/sandboxes/{id}`), not the raw path.
- `sandbox_http_request_duration_seconds{role,route,method}` — request latency histogram.
- `sandbox_sandbox_operations_total{role="api",operation,result}` — sandbox lifecycle ops (`create`, `delete`).
- `sandbox_sandboxes_active{role="api"}` — current sandbox count from the store.
- `sandbox_runners_registered{role="api"}` — runners registered with the API.
- `go_*` and `process_*` — Go runtime and process collectors.

**Response:** `200 OK` with `Content-Type: text/plain; version=0.0.4`.

**Example:**

```bash
curl http://localhost:8080/metrics
```

---

### GET /sandboxes

List sandboxes, ordered by creation time (newest first).

- **Admin key:** all sandboxes
- **Tenant key:** only sandboxes owned by that tenant

**Response:** `200 OK`

```json
[
  {
    "id": "uuid",
    "status": "string",
    "created_at": 1700000000,
    "last_active_at": 1700000000
  }
]
```

**Example:**

```bash
curl http://localhost:8080/sandboxes \
  -H "X-Api-Key: YOUR_API_KEY"
```

---

### POST /sandboxes

Create a new sandbox. No request body is required.

With a **tenant** key, the sandbox is owned by that tenant and counts toward the tenant's `max_sandboxes` quota (`403` when exceeded). With an **admin** key, the sandbox has no tenant owner (admin-visible only for ownership checks; admins see all sandboxes in list).

Resource limits (memory, CPU, process count) are configured on the runner via environment variables. Network policy blocks all private IP ranges and allows public internet access.

**Response:** `201 Created`

```json
{
  "id": "uuid",
  "status": "string",
  "created_at": 1700000000,
  "last_active_at": 1700000000
}
```

**Errors:** `503` when no sandbox runners are registered or available

**Example:**

```bash
curl -X POST http://localhost:8080/sandboxes \
  -H "X-Api-Key: YOUR_API_KEY"
```

---

### GET /sandboxes/{id}

Get sandbox details.

This is a read-only status check: it does not update `last_active_at` or extend idle timers. Only traffic that reaches the runner (exec, files, etc.) counts as activity.

**Path Parameters:**
- `id` — Sandbox UUID

**Response:** `200 OK`

```json
{
  "id": "uuid",
  "status": "string",
  "created_at": 1700000000,
  "last_active_at": 1700000000
}
```

**Errors:** `400` invalid id, `404` not found

**Example:**

```bash
curl http://localhost:8080/sandboxes/550e8400-e29b-41d4-a716-446655440000 \
  -H "X-Api-Key: YOUR_API_KEY"
```

---

### DELETE /sandboxes/{id}

Delete a sandbox.

**Path Parameters:**
- `id` — Sandbox UUID

**Response:** `204 No Content`

**Errors:** `400` invalid id

**Example:**

```bash
curl -X DELETE http://localhost:8080/sandboxes/550e8400-e29b-41d4-a716-446655440000 \
  -H "X-Api-Key: YOUR_API_KEY"
```

---

### POST /sandboxes/{id}/executions

Execute a command in a sandbox. The command runs in a **daemon-side execution** whose
lifetime is independent of the HTTP stream — disconnecting does not kill the process.
Response is streamed as newline-delimited JSON.

**Path Parameters:**
- `id` — Sandbox UUID

**Request Body:**

```json
{
  "command": "echo hello",
  "env": {"KEY": "value"},
  "workdir": "/home",
  "timeout_ms": 300000,
  "exec_id": "client-generated-uuid"
}
```

| Field              | Type              | Required | Default        |
|--------------------|-------------------|----------|----------------|
| `command`          | string            | yes      |                |
| `env`              | map[string]string | no       | `{}`           |
| `workdir`          | string            | no       | `""`           |
| `timeout_ms`       | int64             | no       | `300000` (5m)  |
| `exec_id`          | string            | no       | generated UUID |

The command is always executed via `/bin/sh -c` so that shell features (tilde expansion,
pipes, redirects, etc.) work consistently.

`env` accepts an object of key-value pairs: `{"KEY": "VALUE"}`.

`exec_id`, when provided, sets the execution identifier. If an execution with that ID
already exists, the response follows it instead of starting a new command. This lets the
client define the ID upfront and resume even if the initial connection drops before any
events are received. If omitted, the server generates a UUID.

**Response:** `200 OK` — `Content-Type: application/x-ndjson`

**Response Headers:**
- `X-Exec-Id` — The execution identifier (either the client-supplied `exec_id` or the server-generated UUID). Useful for resuming via the GET endpoint if the stream disconnects before the `started` event arrives.

Stream of JSON objects, one per line. The first event is always a `started` event:

```jsonl
{"seq": 0, "type": "started", "exec_id": "a1b2c3d4-..."}
{"seq": 1, "type": "stdout", "data": "hello\n"}
{"seq": 2, "type": "stderr", "data": "warning: ..."}
{"seq": 3, "type": "exit", "exit_code": 0, "success": true, "execution_time_ms": 42, "timed_out": false, "killed": false}
```

All events include a monotonically increasing `seq` number. The `started` event provides
the `exec_id` needed for the resume and cancel endpoints.

The `exit` event includes:
- `success` — `true` when `exit_code == 0`
- `execution_time_ms` — wall-clock execution time in milliseconds
- `timed_out` — `true` if the process was killed due to timeout
- `killed` — `true` if the process was terminated by a signal

The command runs in a daemon-side execution whose lifetime is independent of the HTTP
stream. Closing the HTTP connection does **not** kill the running command — it only
stops the event stream. To cancel a running command, use
`DELETE /sandboxes/{id}/executions/{exec_id}`. The SDK calls the delete endpoint
automatically when `abortSignal` fires.

The runner automatically retries mid-stream disconnects from the daemon: if the TCP
connection drops before the terminal event, the runner resumes via the daemon's
`GET /executions/{exec_id}?follow=true&after=<seq>` endpoint (up to 3 retries with
exponential backoff starting at 50 ms). The client sees a seamless NDJSON stream.

The execution stores events in a bounded buffer (up to 16 MiB). Clients can reconnect
via `GET /sandboxes/{id}/executions/{exec_id}?after=<seq>&follow=true`. Completed executions
are retained for 10 minutes. If the buffer is exhausted, old events are discarded and
stale resume requests return `410 Gone`.

**Errors:** `400` invalid id or missing command, `404` sandbox not found, `410` if execution exists but history is no longer retained. Transient failures use **503**; **502** means the sandbox is not usable without a client-side change — see [HTTP 503 (transient) vs 502 (not retryable)](#http-503-transient-vs-502-not-retryable).

**Example:**

```bash
curl -X POST http://localhost:8080/sandboxes/550e8400-e29b-41d4-a716-446655440000/executions \
  -H "X-Api-Key: YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"command": "echo hello", "timeout_ms": 10000}'
```

---

### GET /sandboxes/{id}/executions/{exec_id}

Resume or replay an execution stream. Use this to reconnect after a transient
stream disconnect without re-executing the command.

**Path Parameters:**
- `id` — Sandbox UUID
- `exec_id` — Execution ID (from the `started` event)

**Query Parameters:**
- `after` — Sequence number; only events with `seq > after` are returned (default: all events)
- `follow` — `true` to keep the stream open until the command finishes (default: `false`)

When `follow=false`, the endpoint returns retained events as a one-shot snapshot.
When `follow=true`, it streams events until an `exit` or `error` event is sent,
or the client disconnects.

**Response:** `200 OK` — `Content-Type: application/x-ndjson`

Same NDJSON event format as `POST /sandboxes/{id}/executions`.

**Errors:** `400` invalid parameters, `404` execution not found, `410` requested history is no longer retained

**Example:**

```bash
# Resume from sequence 5
curl "http://localhost:8080/sandboxes/550e8400-e29b-41d4-a716-446655440000/executions/a1b2c3d4-...?after=5&follow=true" \
  -H "X-Api-Key: YOUR_API_KEY"
```

---

### DELETE /sandboxes/{id}/executions/{exec_id}

Delete an execution. Kills the running process (if still active) and immediately
removes the execution state from memory. After deletion, the execution can no
longer be resumed or queried. The operation is idempotent — deleting a
nonexistent execution returns `204`.

**Path Parameters:**
- `id` — Sandbox UUID
- `exec_id` — Execution ID (from the `started` event)

**Response:** `204 No Content`

**Errors:** `400` invalid id, `404` sandbox not found

**Example:**

```bash
curl -X DELETE http://localhost:8080/sandboxes/550e8400-e29b-41d4-a716-446655440000/executions/a1b2c3d4-... \
  -H "X-Api-Key: YOUR_API_KEY"
```

---

### GET /sandboxes/{id}/files

List files in a sandbox directory.

**Path Parameters:**
- `id` — Sandbox UUID

**Query Parameters:**
- `path` — Directory path (default: `/`)
- `recursive` — `true` to list recursively (default: `false`)
- `extension` — Filter by file extension, e.g. `.ts` (default: none)

**Response:** `200 OK`

```json
[
  {
    "name": "file.txt",
    "size": 1024,
    "is_dir": false,
    "type": "file",
    "mod_time": "2024-01-01T00:00:00Z"
  }
]
```

**Errors:** `400` invalid id, `404` directory not found

**Example:**

```bash
curl "http://localhost:8080/sandboxes/550e8400-e29b-41d4-a716-446655440000/files?path=/home&recursive=true&extension=.ts" \
  -H "X-Api-Key: YOUR_API_KEY"
```

---

### GET /sandboxes/{id}/files/content

Download a file from a sandbox.

**Path Parameters:**
- `id` — Sandbox UUID

**Query Parameters:**
- `path` — File path (required)

**Response:** `200 OK` — `Content-Type: application/octet-stream`

Raw file contents.

**Errors:** `400` invalid id or missing path, `404` file not found

**Example:**

```bash
curl "http://localhost:8080/sandboxes/550e8400-e29b-41d4-a716-446655440000/files/content?path=/home/user/file.txt" \
  -H "X-Api-Key: YOUR_API_KEY" \
  -o file.txt
```

---

### PUT /sandboxes/{id}/files

Upload (write) a file to a sandbox.

**Path Parameters:**
- `id` — Sandbox UUID

**Query Parameters:**
- `path` — Destination file path (required)
- `overwrite` — `false` to prevent overwriting existing files (default: `true`)

**Request:**
- `Content-Type: application/octet-stream`
- Body: raw file contents
- Max size: 10 MB (configurable via `SANDBOX_API_MAX_FILE_BYTES`)

**Response:** `200 OK`

**Errors:** `400` invalid id or missing path, `409` file exists (when `overwrite=false`)

**Example:**

```bash
curl -X PUT "http://localhost:8080/sandboxes/550e8400-e29b-41d4-a716-446655440000/files?path=/home/user/file.txt" \
  -H "X-Api-Key: YOUR_API_KEY" \
  -H "Content-Type: application/octet-stream" \
  --data-binary @local-file.txt
```

---

### POST /sandboxes/{id}/files

Append data to a file in a sandbox. Creates the file if it doesn't exist.

**Path Parameters:**
- `id` — Sandbox UUID

**Query Parameters:**
- `path` — File path (required)

**Request:**
- `Content-Type: application/octet-stream`
- Body: raw data to append
- Max size: 10 MB (configurable via `SANDBOX_API_MAX_FILE_BYTES`)

**Response:** `200 OK`

**Errors:** `400` invalid id or missing path, `404` path not found

**Example:**

```bash
curl -X POST "http://localhost:8080/sandboxes/550e8400-e29b-41d4-a716-446655440000/files?path=/home/user/log.txt" \
  -H "X-Api-Key: YOUR_API_KEY" \
  -H "Content-Type: application/octet-stream" \
  --data-binary "new log line\n"
```

---

### DELETE /sandboxes/{id}/files

Delete a file or directory from a sandbox.

**Path Parameters:**
- `id` — Sandbox UUID

**Query Parameters:**
- `path` — File or directory path (required)
- `recursive` — `true` to remove non-empty directories (default: `false`)
- `force` — `true` to ignore "not found" errors (default: `false`)

**Response:** `204 No Content`

**Errors:** `400` invalid id or missing path

**Example:**

```bash
curl -X DELETE "http://localhost:8080/sandboxes/550e8400-e29b-41d4-a716-446655440000/files?path=/home/user/dir&recursive=true&force=true" \
  -H "X-Api-Key: YOUR_API_KEY"
```

---

### POST /sandboxes/{id}/files/copy

Copy a file or directory within a sandbox.

**Path Parameters:**
- `id` — Sandbox UUID

**Request Body:**

```json
{
  "src": "/home/user/file.txt",
  "dest": "/home/user/file-copy.txt",
  "recursive": false,
  "overwrite": false
}
```

| Field       | Type   | Required | Default |
|-------------|--------|----------|---------|
| `src`       | string | yes      |         |
| `dest`      | string | yes      |         |
| `recursive` | bool   | no       | `false` |
| `overwrite` | bool   | no       | `false` |

**Response:** `200 OK`

**Errors:** `400` invalid id, missing src/dest, `404` source not found, `409` destination exists

**Example:**

```bash
curl -X POST http://localhost:8080/sandboxes/550e8400-e29b-41d4-a716-446655440000/files/copy \
  -H "X-Api-Key: YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"src": "/home/file.txt", "dest": "/tmp/file.txt", "overwrite": true}'
```

---

### POST /sandboxes/{id}/files/move

Move (rename) a file or directory within a sandbox.

**Path Parameters:**
- `id` — Sandbox UUID

**Request Body:**

```json
{
  "src": "/home/user/old.txt",
  "dest": "/home/user/new.txt",
  "overwrite": false
}
```

| Field       | Type   | Required | Default |
|-------------|--------|----------|---------|
| `src`       | string | yes      |         |
| `dest`      | string | yes      |         |
| `overwrite` | bool   | no       | `false` |

**Response:** `200 OK`

**Errors:** `400` invalid id, missing src/dest, `404` source not found, `409` destination exists

**Example:**

```bash
curl -X POST http://localhost:8080/sandboxes/550e8400-e29b-41d4-a716-446655440000/files/move \
  -H "X-Api-Key: YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"src": "/home/old.txt", "dest": "/home/new.txt"}'
```

---

### POST /sandboxes/{id}/mkdir

Create a directory in a sandbox.

**Path Parameters:**
- `id` — Sandbox UUID

**Query Parameters:**
- `path` — Directory path (required)
- `recursive` — `true` to create parent directories as needed (default: `false`)

**Response:** `201 Created`

**Errors:** `400` invalid id or missing path, `409` directory already exists

**Example:**

```bash
curl -X POST "http://localhost:8080/sandboxes/550e8400-e29b-41d4-a716-446655440000/mkdir?path=/home/user/newdir&recursive=true" \
  -H "X-Api-Key: YOUR_API_KEY"
```

---

### GET /sandboxes/{id}/stat

Get file or directory metadata.

**Path Parameters:**
- `id` — Sandbox UUID

**Query Parameters:**
- `path` — File or directory path (required)

**Response:** `200 OK`

```json
{
  "name": "file.txt",
  "path": "/home/user/file.txt",
  "type": "file",
  "size": 1024,
  "created_at": "2024-01-01T00:00:00Z",
  "modified_at": "2024-01-01T00:00:00Z"
}
```

`exists()` can be derived: a `200` means the file exists, a `404` means it doesn't.

**Errors:** `400` invalid id or missing path, `404` file not found

**Example:**

```bash
curl "http://localhost:8080/sandboxes/550e8400-e29b-41d4-a716-446655440000/stat?path=/home/user/file.txt" \
  -H "X-Api-Key: YOUR_API_KEY"
```

---

## Admin: tenants and API keys

All `/admin/*` routes require an **admin** API key (`SANDBOX_API_KEYS`). Tenant keys receive `403`.

### GET /admin/tenants

List tenants.

**Response:** `200 OK`

```json
[
  {
    "id": "uuid",
    "name": "string",
    "external_ref": "string",
    "max_sandboxes": 50,
    "created_at": 1700000000
  }
]
```

### POST /admin/tenants

Create a tenant. By default also mints one API key (`create_key` defaults to `true`).

**Request body (optional):**

```json
{
  "name": "my-instance",
  "external_ref": "n8n-instance-id",
  "max_sandboxes": 50,
  "create_key": true
}
```

`max_sandboxes` defaults to `50`; `0` means unlimited.

**Response:** `201 Created`

```json
{
  "tenant": {
    "id": "uuid",
    "name": "my-instance",
    "external_ref": "n8n-instance-id",
    "max_sandboxes": 50,
    "created_at": 1700000000
  },
  "key": {
    "id": "uuid",
    "tenant_id": "uuid",
    "prefix": "a1b2c3d4",
    "created_at": 1700000000,
    "api_key": "sbk_a1b2c3d4_…"
  }
}
```

The plaintext `api_key` is only returned on create. Store it securely.

**Example:**

```bash
curl -X POST http://localhost:8080/admin/tenants \
  -H "X-Api-Key: ADMIN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"name":"cloud-instance-1","external_ref":"inst_123"}'
```

### GET /admin/tenants/{id}

Get a tenant by id.

### DELETE /admin/tenants/{id}

Delete a tenant and revoke its API keys (`204`). Existing sandboxes for that tenant remain until deleted/reaped; they become inaccessible to tenant keys.

### GET /admin/tenants/{id}/keys

List API key metadata for a tenant (no plaintext secrets).

### POST /admin/tenants/{id}/keys

Mint an additional API key for the tenant. Returns plaintext `api_key` once (`201`).

### DELETE /admin/tenants/{id}/keys/{keyId}

Revoke an API key (`204`). Revoked keys are rejected immediately.
