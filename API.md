# Sandbox Service API

All endpoints except `/healthz` require the `X-Api-Key` header for authentication.

## Error Response Format

```json
{
  "error": "string",
  "code": 400
}
```

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

### GET /sandboxes

List all sandboxes, ordered by creation time (newest first).

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

Resource limits (memory, CPU, process count) are configured on the runner via environment variables. Network policy blocks all private IP ranges and allows public internet access.

**Response:** `201 Created`

```json
{
  "id": "uuid",
  "status": "string",
  "provider": "delhi",
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

**Path Parameters:**
- `id` — Sandbox UUID

**Response:** `200 OK`

```json
{
  "id": "uuid",
  "status": "string",
  "provider": "delhi",
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

### POST /sandboxes/{id}/exec

Execute a command in a sandbox. Response is streamed as newline-delimited JSON.

**Path Parameters:**
- `id` — Sandbox UUID

**Request Body:**

```json
{
  "command": "echo hello",
  "env": {"KEY": "value"},
  "workdir": "/home",
  "timeout_ms": 300000
}
```

| Field        | Type                          | Required | Default        |
|--------------|-------------------------------|----------|----------------|
| `command`    | string                        | yes      |                |
| `env`        | map[string]string             | no       | `{}`           |
| `workdir`    | string                        | no       | `""`           |
| `timeout_ms` | int64                         | no       | `300000` (5m)  |

The command is always executed via `/bin/sh -c` so that shell features (tilde expansion,
pipes, redirects, etc.) work consistently.

`env` accepts an object of key-value pairs: `{"KEY": "VALUE"}`.

**Response:** `200 OK` — `Content-Type: application/x-ndjson`

Stream of JSON objects, one per line:

```jsonl
{"type": "stdout", "data": "hello\n"}
{"type": "stderr", "data": "warning: ..."}
{"type": "exit", "exit_code": 0, "success": true, "execution_time_ms": 42, "timed_out": false, "killed": false}
{"type": "error", "error": "something went wrong"}
```

The `exit` event includes:
- `success` — `true` when `exit_code == 0`
- `execution_time_ms` — wall-clock execution time in milliseconds
- `timed_out` — `true` if the process was killed due to timeout
- `killed` — `true` if the process was terminated by a signal

**Errors:** `400` invalid id or missing command

**Example:**

```bash
curl -X POST http://localhost:8080/sandboxes/550e8400-e29b-41d4-a716-446655440000/exec \
  -H "X-Api-Key: YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"command": "echo hello", "timeout_ms": 10000}'
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

