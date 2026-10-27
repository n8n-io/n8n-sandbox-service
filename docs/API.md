# Sandbox Service API

All endpoints except `/healthz` and `/metrics` require the `X-Api-Key` header for authentication. `/metrics` is only exposed when `SANDBOX_API_METRICS_ENABLED=true` and is intended to be scraped over a private network; `SANDBOX_API_METRICS_LISTEN_ADDR` can move it to a port of its own, off the port that serves this API.

### API keys and tenants

There are four key classes:

- `SANDBOX_API_KEYS` are admin keys. An admin key can create/list/delete any sandbox, manage tenants and mint tenant API keys via the admin API.
- `SANDBOX_API_PROVISIONER_KEYS` are provisioner keys: `POST /admin/tenants`, `PATCH /admin/tenants/{id}` and `DELETE /admin/tenants/{id}` only, `403` everywhere else. Why the class exists and what it does not protect: [security-model.md](security-model.md#provisioner-keys).
- `SANDBOX_API_SCALER_KEYS` are scaler keys: `GET /admin/runners` only, `403` on every other path ([security-model.md](security-model.md#scaler-keys)).
- Tenant keys are minted by an admin (or returned when a tenant is created). A tenant key may only create sandboxes for that tenant and may only list/get/delete/proxy its own sandboxes. Self-hosted operators can ignore tenant APIs entirely and keep using the admin key.

Tenant keys are returned in plaintext once on create; only a hash is stored.

### Request tracing

Every request carries a trace id through the API, the runner, and the runner's
sandbox lifecycle events. Clients may send a [W3C `traceparent`](https://www.w3.org/TR/trace-context/#traceparent-header)
header to join their own trace:

```
traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01
```

The header is optional. A missing or malformed value is replaced with a freshly
generated trace context, and a value from a later spec version is reduced to the
fields above, so a caller cannot make the service log or relay content of their
choosing. The trace id is not returned in responses; it appears in the service's
log events. See [observability.md](observability.md).

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
Every `503` and `429` the API sends carries `Retry-After` (seconds; `1` unless the API chose another), including those relayed from a runner.

- **502 Bad Gateway** — **Not retryable as “wait and retry”**: the request does not make sense to repeat unchanged; fix state first (new sandbox, repair registry/routing, or handle the reported error). Examples: stored sandbox has **no runner HTTP base URL** or **delete** failed on the runner control plane.

### HTTP 429 — too many requests in progress

Two limits cap how many requests can be in progress at the same time. Neither is a rate.

- **Per tenant**, in the API: all of a tenant's keys together, counted on each API replica. Admin keys are not counted.
- **Per sandbox**, in the runner: exec and file requests, from all API replicas together.

Their sizes are `SANDBOX_API_MAX_INFLIGHT_PER_TENANT` and `SANDBOX_RUNNER_MAX_INFLIGHT_PER_SANDBOX` in [configuration.md](configuration.md). A request over either is refused before it reaches the sandbox:

```
HTTP/1.1 429 Too Many Requests
Retry-After: 1

{"error":"too many requests in progress for this tenant","code":429,"reason":"tenant_request_limit"}
```

`reason` is `tenant_request_limit` or `sandbox_request_limit`; the per-sandbox body has no `code`. A stream counts until it ends, a create until the runner has finished it. Execution `DELETE`s have limits of their own, counted the same way: 16 per tenant and 4 per sandbox, so work can be cancelled while the others are full. Safe to retry after the delay.

### HTTP 409 `sandbox_restarted` — the sandbox came back without its memory

A sandbox whose guest crashed is recovered automatically by booting its existing filesystem. Files survive; everything that was in memory does not. The request that triggered the recovery waits for it, then fails with `409`, so the loss is reported once instead of silently:

```
HTTP/1.1 409 Conflict
X-Sandbox-Restarted: 1

{"error":"sandbox restarted after guest crash; state in memory was lost","reason":"sandbox_restarted"}
```

Retry once. The sandbox is already running when this arrives, so there is no `Retry-After` and nothing to wait for. Do not retry it silently: the point of the status is that the client learns its sandbox is not the one it left behind.

Match on `X-Sandbox-Restarted: 1`, which survives both proxy hops and is listed in `Access-Control-Expose-Headers` so browser clients can read it, or on the body's `reason`, the only field that tells this `409` from any other. Do not look for a `code` in it: the runner writes this body and the API proxies it through untouched, so unlike the shape above it carries `error` and `reason` only, and the integer status appears in the status line alone. Every request the crash stranded sees it, since one recovery serves them all. Later requests are ordinary `200`s, and `status` in `GET /sandboxes/{id}` stays `running` throughout.

What the client loses:

- Running processes. A dev server or worker started by an earlier execution is dead and nothing restarts it; its files remain, so relaunch it.
- Completed executions. History is in memory only, so `GET /sandboxes/{id}/executions/{exec_id}` returns `404 execution not found` even for a command that finished before the crash. `DELETE` of one returns `204` instead, since the crash already did what the delete asks for.
- Idempotency of a caller-supplied `exec_id`. Re-posting an id that ran before the restart runs the command again instead of returning the earlier result.
- Writes that had not reached the disk. On Firecracker the crash is a guest kernel panic, so it takes the guest page cache with it, and neither a shell redirect nor `PUT /files` flushes. A file written seconds before the crash can be missing or truncated where an older one is intact. Run `sync` in the sandbox if a write has to survive a crash it is racing.

An ordinary wake from an idle stop stays transparent: no `409`, no header. On the Sysbox runtime that wake costs the same three things, because a stopped container is started again rather than resumed; on Firecracker an idle stop snapshots the paused guest, so memory does survive it. The `409` is not what tells them apart — `status` is. An idle stop sets `status` to `stopped`, so a client can see it in `GET /sandboxes/{id}` and know what it lost, whereas a crash leaves `status` at `running`. The `409` exists for the case that has no other signal. An ephemeral sandbox never reaches `stopped`; it is deleted instead (see `ephemeral` under `POST /sandboxes`).

**404** `sandbox not found` — Unknown id, the sandbox is past its idle delete-after wake window (`SANDBOX_API_IDLE_DELETE_AFTER`, default `24h`; the idle-stop window for an ephemeral sandbox, see `POST /sandboxes`), or the runner no longer tracks the sandbox (eviction, delete, or runner restart). On exec/file proxy routes, when the runner signals sandbox gone (`X-Sandbox-Gone: 1`), the API removes the store row so subsequent `GET /sandboxes/{id}` also returns 404. Other runner **404** responses (for example `execution not found` or missing file paths) do **not** delete the sandbox. Exec and file routes may return **503** or **502** from the runner after the API successfully reaches the runner; the API may return **503** `runner unavailable` before the runner is contacted.

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

Served on the port that serves this API unless `SANDBOX_API_METRICS_LISTEN_ADDR` names another one, in which case a dedicated listener serves it and this path is a `404` here. See [configuration.md](configuration.md#metrics).

Metric families include:

- `sandbox_http_requests_total{role,route,method,status}` — request counter; `route` is the matched route pattern (e.g. `/sandboxes/{id}`), not the raw path.
- `sandbox_http_request_duration_seconds{role,route,method}` — request latency histogram.
- `sandbox_sandbox_operations_total{role="api",operation,result}` — sandbox lifecycle ops (`create`, `delete`).
- `sandbox_create_placements_total{role="api",result}` — how each create found a runner that did not refuse it for capacity: `first_choice`, or `retried` after a refusal; or found none: `exhausted` (every runner tried refused) or `no_runner` (none eligible). Whether the create then succeeded is in `sandbox_sandbox_operations_total`.
- `sandbox_sandboxes_active{role="api"}` — current sandbox count from the store.
- `sandbox_runners_registered{role="api"}` — runners registered with the API.
- `go_*` and `process_*` — Go runtime and process collectors.

**Response:** `200 OK` with `Content-Type: text/plain; version=0.0.4`.

**Example:**

```bash
curl http://localhost:8080/metrics
# with SANDBOX_API_METRICS_LISTEN_ADDR=127.0.0.1:9100
curl http://127.0.0.1:9100/metrics
```

---

### GET /sandboxes

List sandboxes, ordered by creation time (newest first).

- Admin key: all sandboxes, or one owner's with `?tenant_id=`
- Tenant key: only sandboxes owned by that tenant

Sandboxes past their idle-delete window stay listed until the sweeper removes them, even though `GET /sandboxes/{id}` already returns `404` for them.

**Query parameters:**

- `tenant_id` (admin only) — a tenant UUID, or `__admin__` for sandboxes created with an admin key. An unknown tenant yields `[]`.

**Response:** `200 OK`

```json
[
  {
    "id": "uuid",
    "status": "string",
    "created_at": 1700000000,
    "last_active_at": 1700000000,
    "ephemeral": false,
    "egress": "public",
    "template": "default",
    "tenant_id": "uuid"
  }
]
```

`tenant_id` is the owning tenant, or `__admin__` for an admin-owned sandbox. A tenant key only ever sees its own id.

**Errors:** `400` `tenant_id` is neither a UUID nor `__admin__`, or is given more than once, `403` `tenant_id` sent with a tenant key

**Examples:**

```bash
curl http://localhost:8080/sandboxes \
  -H "X-Api-Key: YOUR_API_KEY"

# One tenant's sandboxes (admin key)
curl "http://localhost:8080/sandboxes?tenant_id=0f0e0d0c-0000-4000-8000-000000000001" \
  -H "X-Api-Key: ADMIN_KEY"
```

---

### POST /sandboxes

Create a sandbox. With no request body, the service generates a UUID. Callers may instead supply a lowercase UUID to create or reconnect to a deterministic sandbox:

```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000"
}
```

If that ID still belongs to the caller and is within its idle-delete window, the existing sandbox is returned, or `409` if the request names an `egress`, `egress_proxy`, `template`, `idle_stop_after` or `idle_delete_after` that differs from the sandbox's, with `reason` `egress_mismatch`, `egress_proxy_mismatch`, `template_mismatch` or `idle_window_mismatch`. A field left out matches whatever the sandbox has. If it has passed the window, the stale sandbox is deleted before the ID is reused. If its runner no longer has it (a request was answered `404` with `X-Sandbox-Gone`, for example after its container was removed out of band), the create makes a new sandbox under the ID; the runner drops what it still held for the old one. If the ID belongs to another tenant (or to admin when the caller is a tenant), the request fails with `409`.

Disconnecting before the response does not cancel a create a runner has accepted: if it succeeds, the sandbox is still created and tracked, and can be found by listing or by repeating the request with the same `id`. A disconnect after a runner refused it (below) ends the create with nothing created.

The sandbox goes to the eligible runner with the lowest reported `capacity_used`. A full runner refuses before creating anything, and the API tries the next one: up to three runners, none started more than 5 seconds after the first. Any other runner error ends the create with `500`. When every runner tried refuses, the answer is:

```
HTTP/1.1 503 Service Unavailable
Retry-After: 1

{"error":"sandbox runners are at capacity","code":503,"reason":"no_capacity"}
```

When no runner is eligible, it is the same except for `error`: `no sandbox runners are registered or available`. Either way nothing was created, so the request is safe to repeat even without an `id`. Match on `reason`.

**Request body fields** (all optional):

- `id` — lowercase UUID to create or reconnect to, as above.
- `ephemeral` (boolean, default `false`) — delete the sandbox, instead of stopping it, once it has been idle for `SANDBOX_API_IDLE_STOP_AFTER` (default `1h`; `SANDBOX_API_IDLE_DELETE_AFTER` when idle stop is disabled, never when both are `0`). It never reports `status: "stopped"`; once the window has passed, `GET`, exec and file requests return `404` and the sweeper removes it. Fixed at creation: reconnecting to an existing `id` keeps the value it was created with.

```json
{
  "ephemeral": true
}
```

- `egress` (string, default `"public"`) — outbound network policy. `"public"`: the internet, minus the [blocked destinations](security-model.md#guest-to-host-and-network). `"none"`: nothing leaves the sandbox, DNS included; the API still reaches it. `"proxy"`: only what `egress_proxy` allows, through the runner's [egress proxy](#the-egress-proxy); a runner that cannot serve it answers `501` `not_implemented` and nothing is created. Fixed at creation: a reconnect to an existing `id` that names a different mode is refused with `409`, so a caller asking for `"none"` never gets an open sandbox back.
- `egress_proxy` (object) — required with `egress: "proxy"`, refused otherwise. Unknown fields are refused.

```json
{
  "egress": "proxy",
  "egress_proxy": {
    "allow": [
      { "name": "git", "hosts": ["git.example.com", "forgejo:3000"] },
      { "name": "anthropic", "hosts": ["api.anthropic.com"] }
    ],
    "repos": [{ "host": "git.example.com", "path": "org/repo", "access": "write" }],
    "push_refs": ["refs/heads/factory/i1/", "refs/heads/factory-wip/s1"],
    "hide_refs": ["refs/heads/factory/", "refs/heads/factory-wip/"],
    "show_refs": ["refs/heads/factory/i1/", "refs/heads/factory-wip/i1."],
    "credentials": [
      { "host": "git.example.com", "secret_ref": "factory/git-token", "scheme": "basic", "username": "factory-agent" },
      { "host": "api.anthropic.com", "secret_ref": "factory/anthropic-key", "scheme": "header", "header": "x-api-key" }
    ],
    "model_hosts": [{ "host": "api.anthropic.com", "provider": "anthropic" }],
    "labels": { "instance_id": "i1", "session_id": "s1" }
  }
}
```

  - `allow` (required, up to 64; `[]` refuses every host, a missing or `null` one is `400`): named groups of 1–16 hosts each. A host is a lowercase DNS name with an optional `:port` other than `80` or `443` (the defaults, left out); IP addresses (hex forms included) and wildcards are refused.
  - `allow_public` (boolean): also let through any other host on port 443 (CONNECT) or 80 (plain HTTP) whose every address is public, tunnelled uninspected: no credential, repo filter or metering applies to a host `allow` does not name, and `SANDBOX_EGRESS_PROXY_INTERNAL_HOSTS` exempts only hosts `allow` names. `400` `allow_public_disabled` unless the service sets `SANDBOX_API_EGRESS_ALLOW_PUBLIC`; turning that off leaves existing sandboxes' specs as they are.
  - `repos` (up to 64): Git repositories on an `allow` host. `path` is lowercase, without `.git`. `access` is `read` (fetch) or `write` (fetch and push to `push_refs`).
  - `push_refs` (up to 16): refs under `refs/heads/`; one ending in `/` is a prefix, any other is one ref.
  - `hide_refs` (up to 16): refs under `refs/` that fetches from `repos` must not see; one ending in `/` or `.` is a prefix, any other is one ref. See [hidden refs](#hidden-refs).
  - `show_refs` (up to 16, needs `hide_refs`): exceptions to `hide_refs`, in the same form.
  - `credentials` (up to 32): per `allow` host, a reference to the secret to inject. Only the reference is stored. `scheme` is `bearer` (default), `basic` (needs `username`) or `header` (needs `header`, a lowercase name that is not hop-by-hop, `host`, `cookie`, `content-*` or `proxy-*`); `allow_plain_http` (boolean).
  - `model_hosts` (up to 8): `allow` hosts that are model APIs, with `provider` `anthropic` or `openai`. A host with `repos` cannot be one.
  - `labels` (up to 16): string map; keys lowercase (`a-z0-9_.-`, at most 63), values 1–128 of `A-Za-z0-9._:@/-`. Copied into usage records and refusal logs.

  Change the spec later with [`PUT /sandboxes/{id}/egress-proxy`](#put-sandboxesidegress-proxy).
- `template` (string, default `"default"`) — image the sandbox boots, one of [`GET /templates`](#get-templates) (`SANDBOX_API_TEMPLATES`); any other name is `400` with `reason` `unknown_template`. The sandbox goes only to a runner that serves it; when none that does is eligible or has room, the answer is `503` `no_capacity` as above. Fixed at creation.
- `idle_stop_after`, `idle_delete_after` (integer seconds) — per-sandbox idle windows that replace `SANDBOX_API_IDLE_STOP_AFTER` and `SANDBOX_API_IDLE_DELETE_AFTER` for this sandbox, even when those are `0`. From 60 up to the tenant's `max_idle_window` or, without one (and for admin keys), `SANDBOX_API_MAX_IDLE_WINDOW` (default `24h`), with `idle_stop_after` no larger than `idle_delete_after`. An ephemeral sandbox is deleted at its `idle_stop_after`. A sandbox is only deleted once stopped, so an `idle_delete_after` shorter than the stop window in force waits for the stop. Change them with [`PATCH /sandboxes/{id}`](#patch-sandboxesid).
- `idle_archive_after` (integer seconds) — idle time after which a stopped sandbox is [archived](#archive-and-restore), replacing `SANDBOX_API_IDLE_ARCHIVE_AFTER`; bounded like the other windows, between `idle_stop_after` and `idle_delete_after` when those are set. Needs an archive store; refused with `ephemeral`.

With a tenant key, the sandbox is owned by that tenant and counts toward the tenant's `max_sandboxes` quota (`403` when exceeded). With an admin key, the sandbox is stored with `tenant_id` `__admin__` (admin-owned; not visible to tenant keys; admins see all sandboxes in list).

The quota holds under concurrent creates. Before the runner call, each API pod counts the tenant's saved sandboxes plus its own creates still in progress, so on one pod no create past the quota reaches a runner while the quota stays the same. The quota is checked again when the sandbox is saved, where a tenant's creates are counted one at a time. That check catches a burst spread over several API pods and a quota lowered during a create. A create refused there was already made on a runner; the API deletes it there before the `403`, and if that delete fails, the sandbox stays on the runner untracked. Either way the tenant can briefly hold more sandboxes on runners than its quota.

Resource limits (memory, CPU, process count) are configured on the runner via environment variables.

**Response:** `201 Created` for a new sandbox, or `200 OK` when returning an existing caller-supplied sandbox

```json
{
  "id": "uuid",
  "status": "string",
  "created_at": 1700000000,
  "last_active_at": 1700000000,
  "ephemeral": false,
  "egress": "public",
  "template": "default",
  "tenant_id": "uuid"
}
```

`idle_stop_after` and `idle_delete_after` are included when the create set them. `egress_proxy` is not returned.

**Errors:** `400` invalid request body (over 64 KiB, or any field above out of range), supplied id, `403` tenant sandbox quota exceeded (when creates on other API pods filled it first, the runner create is rolled back), `409` if the supplied id is owned by another tenant/admin, exists with a different option than requested (see above), or the tenant was deleted before the sandbox row could be stored (runner create is rolled back), `429` [too many requests in progress](#http-429--too-many-requests-in-progress), `502` stale sandbox cleanup failed, or the runner did not confirm the requested `egress` (runner create is rolled back; a runner from before egress modes only serves `public`), `400` `unknown_secret_ref`: a credential names a secret the tenant does not have, `400` `allow_public_disabled`: `allow_public` without `SANDBOX_API_EGRESS_ALLOW_PUBLIC`, `501` `not_implemented`: `egress: "proxy"` on a runner without proxy support, `model_hosts` on one with no usage sink, or an option a runner refuses because it predates it, `503` `no_capacity`: no runner is eligible or has room (see above)

**Examples:**

```bash
# Server-generated ID
curl -X POST http://localhost:8080/sandboxes \
  -H "X-Api-Key: YOUR_API_KEY"

# Caller-supplied deterministic ID
curl -X POST http://localhost:8080/sandboxes \
  -H "X-Api-Key: YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"id":"550e8400-e29b-41d4-a716-446655440000"}'

# Ephemeral
curl -X POST http://localhost:8080/sandboxes \
  -H "X-Api-Key: YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"ephemeral":true}'

# No egress
curl -X POST http://localhost:8080/sandboxes \
  -H "X-Api-Key: YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"egress":"none"}'
```

---

### The egress proxy

An `egress: "proxy"` sandbox reaches nothing but the egress proxy its runner serves: a TCP port on its network gateway (Docker: a bridge of its own; Firecracker: inside the slot's network namespace). The guest has no DNS; the proxy resolves names. Every execution gets `HTTPS_PROXY`, `HTTP_PROXY` (and lowercase forms) and `NO_PROXY=localhost,127.0.0.1,::1` unless the request sets them, and `SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE`, `CURL_CA_BUNDLE`, `GIT_SSL_CAINFO` (the guest's bundle plus the runner's egress CA, at `/tmp/sandbox-egress/ca-bundle.pem`) and `NODE_EXTRA_CA_CERTS` (the CA alone, `/tmp/sandbox-egress/ca.pem`).

- Destinations: CONNECT or absolute-form plain HTTP to an `allow` host exactly, port 443 or 80 when the entry has none, or with `allow_public` any host on 443 or 80. Every A/AAAA record is checked against the blocked ranges (the service's IPv4 list, `0.0.0.0/8`, IPv6 loopback, unique-local, link-local, multicast and the unspecified address, and mapped or NAT64 forms of blocked IPv4 addresses) unless the host is in `SANDBOX_EGRESS_PROXY_INTERNAL_HOSTS` and named in `allow`, and the proxy dials the address it checked.
- Hosts named in `repos`, `credentials` or `model_hosts` are inspected: the proxy terminates TLS with a certificate from the runner's CA for the CONNECT target, whatever SNI says, offers only HTTP/1.1, and serves each request separately, re-serialised. A request whose `Host` is not the CONNECT target, an `Upgrade`, or an `Expect` other than `100-continue` is refused. Upstream TLS is verified for the target's name; redirects are returned to the guest, never followed. Other hosts are tunnelled untouched.
- A host with `repos` forwards only Git smart HTTP for listed repositories: `info/refs?service=git-upload-pack|git-receive-pack` and `POST …/git-upload-pack|git-receive-pack`, path compared ignoring ASCII case and a `.git` suffix, with no `%`-escape, empty or dot segment, and no other query. Push needs `write`; its command list (shallow lines, then `<old> <new> <ref>` lines, then a flush; a gzip body is decoded) must name only refs matching `push_refs`, or the whole push is refused before any of it is forwarded. `push-cert` is removed from the receive-pack advertisement. With `hide_refs`, fetches are filtered too ([hidden refs](#hidden-refs)).
- A credential replaces whatever the guest sent in its header, and is sent over plain HTTP only with `allow_plain_http`. Secrets are read from `<SANDBOX_EGRESS_PROXY_SECRETS_DIR>/<tenant_id>/<secret_ref>`, at most 30 s old; none reaches the guest or a log. A credentialed request goes upstream without `Accept-Encoding`, `Range` or `If-Range`, and a response that echoes the secret (or the header value it was sent as, as is or JSON- or percent-escaped) has it overwritten with `*` in its headers and body; a compressed or `206` response to one, which the proxy cannot scrub whole, is `502`. Blanks around a secret file's value are trimmed.
- A sandbox may hold 256 proxy connections at once (more are `429`); a tunnel idle for 10 minutes is closed. A push command list over 1 MiB is refused.
- A model host forwards only `POST /v1/messages`, `POST /v1/messages/count_tokens` and `GET /v1/models` (anthropic) or `POST /v1/chat/completions`, `POST /v1/responses` and `GET /v1/models` (openai). Metered requests go upstream without `Accept-Encoding`, and each yields a usage record (below). An openai `POST /v1/responses` body is read whole (at most 16 MiB) and refused with `background: true`, which would answer before any usage exists. While [model egress](#get-put-sandboxesidmodel-egress) is blocked, every request to a model host is refused.
- A refusal is `403` (`502` for `credential_unavailable`) with `X-Sandbox-Egress-Refusal: <reason>` and `{"error": …, "reason": …}`, in the provider's error shape on a model host. Reasons: `host_not_allowed`, `address_not_allowed`, `path_not_allowed`, `repo_not_allowed`, `ref_not_allowed`, `protocol_not_allowed`, `model_blocked`, `credential_unavailable`. Each is logged on the runner with the sandbox, tenant and labels.

#### Hidden refs

A Git server sends any object it holds to a protocol v2 fetch that names its id, whatever refs it lists (and Forgejo allows any id in v0 too), so with `hide_refs` set the proxy filters every fetch from a `repos` repository. A ref is hidden when it matches `hide_refs` and not `show_refs`.

- Fetches must use protocol v2 (`Git-Protocol: version=2`); anything else is `protocol_not_allowed`. The capability advertisement offers only `ls-refs` and `fetch`; any other command is `path_not_allowed`.
- `ls-refs` answers leave out hidden refs, any name but `HEAD` and `refs/*`, a symref whose target is hidden, and a `HEAD` that is not a symref.
- A `fetch` may `want` only an object id the proxy listed to this sandbox as a visible ref's value or peeled value. When a want is not one, the proxy lists the repository's refs itself (at most once a second per sandbox) and checks again, so a fetch by id that skips `ls-refs` works for a visible tip. An id below a tip is refused even when that tip is visible: the proxy has no object graph. The listed ids are forgotten when the spec changes. `want-ref` of a hidden ref, `deepen-not`, and a `filter` other than `blob:none`, `blob:limit=`, `tree:` or `object:type=` are `ref_not_allowed`; an unknown argument is `path_not_allowed`. A request body is read whole (gzip decoded, at most 4 MiB), and a sandbox has at most 8 such requests in flight; more wait.
- The receive-pack advertisement leaves out hidden refs and `.have` lines.
- `have` lines pass unchecked, so the server's acknowledgments confirm whether it holds an id the guest already names; they never send its content.
- Not covered: a push may name an object it does not send, and a server that holds that object accepts it, so a guest that already knows a hidden object's id can push a commit or tree naming it and fetch it back from its own ref. Nothing the proxy forwards discloses a hidden ref's id; keep data that must stay unreadable in another repository.

Usage records go to `SANDBOX_EGRESS_USAGE_SINK_URL` as `POST {"batch_id", "runner_id", "records": [...]}` with `Authorization: Bearer <SANDBOX_EGRESS_USAGE_SINK_TOKEN_FILE>`, at least every 10 s and at most 500 records a batch. A record has `record_id` (dedupe on it: delivery is at least once), `sandbox_id`, `tenant_id`, `labels`, `provider`, `host`, `path`, `model`, `provider_request_id`, `status`, `started_at`, `ended_at`, `streamed`, `complete` (false when the response ended before its final usage) and `input_tokens`, `output_tokens`, `cache_read_input_tokens`, `cache_creation_input_tokens`, normalised so all input is the sum of the three input fields (for openai, cached tokens are moved out of the input count). `408`, `429` and `5xx` are retried with the same `batch_id` for up to an hour, other statuses drop the batch; records wait in runner memory and are lost if it restarts.

### PUT /sandboxes/{id}/egress-proxy

Replace a proxy sandbox's `egress_proxy`, validated as on create. The runner applies it to every request it receives after the `204` and closes open tunnels to hosts it no longer allows; the stored spec changes once the runner has it, so a reconnect then matches the new one.

**Errors:** `400` invalid spec, `unknown_secret_ref` or `allow_public_disabled`, `404` not found, `409` `egress_mismatch`: not a proxy sandbox, `501` `not_implemented`: the runner cannot serve it, or predates one of its fields, `503` the runner did not answer (with `Retry-After`)

### GET, PUT /sandboxes/{id}/model-egress

Whether new requests to the sandbox's model hosts are blocked (budget cut-off). `PUT {"blocked": true|false}` sets it; requests already streaming finish. Both answer `{"blocked": …}`. `false` at create; the runner holds it in memory, so it lasts until the sandbox or the runner goes.

**Errors:** as for `PUT /sandboxes/{id}/egress-proxy`

---

### GET /templates

Admin or tenant key. The template catalog, in `SANDBOX_API_TEMPLATES` order with `default` first: what one sandbox of each gets, from a runner that serves it, and how many healthy, undrained runners serve it now.

```json
{
  "templates": [
    { "name": "default", "vcpus": 1, "memory_mb": 512, "disk_mb": 2048, "runners": 3 },
    { "name": "python", "vcpus": 4, "memory_mb": 8192, "disk_mb": 20480, "runners": 1 }
  ]
}
```

Sizes are `0` for a template no registered runner serves, and `disk_mb` is `0` on a Docker runner that enforces no disk quota. A template with `runners: 0` is valid in a create, which answers `503` `no_capacity` until a runner serving it is back.

---

### GET /sandboxes/{id}

Get sandbox details.

This is a read-only status check: it does not update `last_active_at` or extend idle timers. Only proxied traffic (exec, files, etc.) counts as activity, and only when the sandbox actually serves it — a request that fails with **502** or **503** leaves `last_active_at` and `status` untouched, so retrying against a broken sandbox cannot hold it open past its idle timers. A **4xx** still counts, since the sandbox was reachable and rejected the request — except a runner sandbox-gone **404**, which never counts as activity even if removing the store row fails. Activity is recorded when the response starts. Beyond that, while an execution is running, or an execution stream (`POST .../executions`, or `GET .../executions/{exec_id}?follow=true`) is open, the runner reports the sandbox active in every heartbeat (every 10 s), and refuses an idle stop for it, so the sweeper never stops a sandbox under a long run. An execution counts until the runner sees it exit, the daemon no longer has it, or its `timeout_ms` plus a minute passes; a runner restart forgets them.

**Path Parameters:**
- `id` — Sandbox UUID

**Response:** `200 OK`

```json
{
  "id": "uuid",
  "status": "string",
  "created_at": 1700000000,
  "last_active_at": 1700000000,
  "ephemeral": false,
  "egress": "public",
  "template": "default",
  "tenant_id": "uuid"
}
```

`status` is `running`, `stopped`, `archived` or `restoring` ([archive and restore](#archive-and-restore)); an ephemeral sandbox is only ever `running`. `egress`, `template` and, when set, `idle_stop_after`, `idle_archive_after` and `idle_delete_after` are the sandbox's; `archived_at` is set while it is archived or restoring. `tenant_id` is the owner, `__admin__` for an admin-owned sandbox.

**Errors:** `400` invalid id, `404` not found

**Example:**

```bash
curl http://localhost:8080/sandboxes/550e8400-e29b-41d4-a716-446655440000 \
  -H "X-Api-Key: YOUR_API_KEY"
```

---

### Archive and restore

With an archive store configured ([configuration.md](configuration.md#archive-store)), a stopped sandbox's disk can leave its runner: `archived` sandboxes hold no runner capacity and survive runner restarts and host loss. What is kept is every file on the guest's disk; memory, processes and execution history are not, and a restored sandbox cold boots.

- A sandbox is archived after `idle_archive_after` (or `SANDBOX_API_IDLE_ARCHIVE_AFTER`) of inactivity once stopped, on [`POST /sandboxes/{id}/archive`](#post-sandboxesidarchive), or during a [drain](#get-post-delete-adminrunnersiddrain) with `{"archive": true}`. Ephemeral sandboxes, and sandboxes with a running execution or open stream, are not archived.
- The runner compresses the disk (Firecracker: its rootfs; Docker: the container's whole filesystem, at most the template's image size plus its disk quota where one applies) and seals it with AES-256-GCM under a key made for that archive alone, so an altered, truncated or reordered object does not open. It uploads to a URL the API signs for that object only; runners hold no store credentials. The API checks the stored size, records the archive (the key wrapped by `SANDBOX_API_ARCHIVE_KEY_FILE`), and only then deletes the runner's copy; any failure before that leaves the sandbox stopped on its runner.
- Any exec or file request for an archived sandbox starts a restore and gets `503` `restoring` with `Retry-After: 5`, as does every request until it is `running` again. The restore goes to a healthy runner serving the sandbox's template at the version it was archived from (one that still has it), and keeps the sandbox's options, egress proxy spec included. A restore is not a crash: no `409 sandbox_restarted`. An archive the store no longer has, or that does not open, deletes the sandbox, and later requests get `404`; a failed download or no runner with room leaves it archived, and a request at least 30 s later retries.
- On Firecracker a stop asks the guest to flush its writes to the rootfs first, waiting up to 30 s. A sandbox whose guest did not is woken and stopped again before it is archived; if the guest still does not flush, its rootfs is archived as it is, as consistent as after a crash.
- Deleting an archived sandbox deletes its archive; `idle_delete_after` applies to archived sandboxes as to stopped ones. A restored sandbox's archive is deleted.

### POST /sandboxes/{id}/archive

Archive now: a running sandbox is stopped first. Answers once the archive is recorded, with the sandbox. Archiving an archived sandbox is a no-op `200`.

**Errors:** `404`, `409` `ephemeral` or `sandbox_busy` (a run in progress), `501` no archive store, `503` the runner or store failed (the sandbox is left stopped on its runner) or `restoring`

### POST /sandboxes/{id}/restore

Start restoring an archived sandbox ahead of use: `503` `restoring` with `Retry-After` while it restores, `200` with the sandbox once it is not archived.

### PATCH /sandboxes/{id}

Change a sandbox's idle windows. The body sets any of `idle_stop_after`, `idle_archive_after` and `idle_delete_after`, with the bounds of [create](#post-sandboxes) checked against the result, including the owning tenant's `max_idle_window` whatever key is used; `null` clears a window back to the global one. Unknown fields return `400`. The new windows apply from the next sweep; a stopped sandbox stays stopped.

```json
{ "idle_stop_after": 1800, "idle_delete_after": 604800 }
```

**Response:** `200 OK` with the sandbox, as `GET /sandboxes/{id}` returns it.

**Errors:** `400` invalid id, empty or invalid body, a window out of bounds, `404` not found or not the caller's

---

### DELETE /sandboxes/{id}

Delete a sandbox.

**Path Parameters:**
- `id` — Sandbox UUID

**Response:** `204 No Content`

**Errors:** `400` invalid id, `404` sandbox not found (unknown id, already deleted, or not owned by the caller — same status as `GET` so tenants cannot probe cross-tenant existence)

**Example:**

```bash
curl -X DELETE http://localhost:8080/sandboxes/550e8400-e29b-41d4-a716-446655440000 \
  -H "X-Api-Key: YOUR_API_KEY"
```

---

### POST /sandboxes/{id}/executions

Execute a command in a sandbox. The command runs in a **daemon-side execution** whose
lifetime is independent of the HTTP stream — disconnecting only stops the event stream,
it does not kill the process; cancel with `DELETE /sandboxes/{id}/executions/{exec_id}`
(the SDK does this when `abortSignal` fires). Response is streamed as newline-delimited JSON.

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

`timeout_ms` may not exceed `SANDBOX_RUNNER_MAX_EXEC_TIMEOUT` ([configuration.md](configuration.md#shared-runner-config)); a larger value gets `400` naming the maximum. Nothing else bounds an execution's length: no proxy hop has a deadline, and the idle sweeper never stops a sandbox while an execution in it is running. A resume can only start from history the daemon still holds, at most `SANDBOX_EXEC_MAX_EVENT_BYTES` per execution, so a long run with a lot of output answers `410` to a resume that fell behind.

The command is always executed via `/bin/sh -c` so that shell features (tilde expansion,
pipes, redirects, etc.) work consistently.

`env` accepts an object of key-value pairs: `{"KEY": "VALUE"}`.

**Sandbox environment.** A command starts with `HOME=/home/user` and this `PATH`:

```
/home/user/venv/bin:/home/user/.npm-global/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
```

`env` layers on top, so a supplied `PATH` replaces this one.

A sandbox has no root and no `sudo`, so `apt-get` does not work. Install packages
unprivileged instead. Each command below writes only under `/home/user`:

| To install | Run |
| --- | --- |
| A PyPI package | `pip install <name>` — `python3` and `pip` come from the virtual environment at `/home/user/venv` |
| A global npm package | `npm install -g <name>` — writes to `/home/user/.npm-global`, whose `bin` is on `PATH` |
| A project npm package | `npm install <name>` in the project directory |

The image carries a C and C++ compiler, `make` and the Python headers, so a
source build works too. It carries no other library's development headers, and a
sandbox cannot add them.

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

Each `stdout` and `stderr` event usually holds one line. An output line longer than 64 KiB
is sent as several events, and only the last one ends with `\n`. Every line ends with `\n`:
a last line without one gets one, and `\r\n` arrives as `\n`. `stdout` and `stderr`
events can interleave, so append the `data` of each event to a separate buffer for its
`type`.

The `exit` event includes:
- `success` — `true` when `exit_code == 0`
- `execution_time_ms` — wall-clock execution time in milliseconds
- `timed_out` — `true` if the process was killed due to timeout
- `killed` — `true` if the process was terminated by a signal

The runner retries mid-stream disconnects from the daemon transparently (resume via the
daemon's `GET /executions/{exec_id}?follow=true&after=<seq>`, up to 3 retries with
exponential backoff from 50 ms). If those retries are exhausted the response body simply
ends; the status line was already `200 OK`, so **a stream that ends without an `exit` or
`error` event has not completed** — treat it as a failure, not an empty result. The same
applies when the client↔API connection drops.

Recovery is the client's job: the execution outlives the stream, so reconnect with
`GET /sandboxes/{id}/executions/{exec_id}?after=<last seq>&follow=true`. The SDK does this
automatically (up to 10 attempts, 250 ms apart) and throws `SandboxServiceError` if the
execution still has not produced an `exit` event.

Events are kept in a bounded buffer (16 MiB per execution; completed executions are
retained for 10 minutes — see `SANDBOX_EXEC_*` in [configuration.md](configuration.md#sandbox-daemon)).
When the buffer is exhausted, old events are discarded and stale resume requests return `410 Gone`.

**Errors:** `400` invalid id, missing command or `timeout_ms` above the maximum, `404` sandbox not found, `410` if execution exists but history is no longer retained, `429` too many requests in progress (see [HTTP 429](#http-429--too-many-requests-in-progress)). Transient failures use **503**; **502** means the sandbox is not usable without a client-side change — see [HTTP 503 (transient) vs 502 (not retryable)](#http-503-transient-vs-502-not-retryable).

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

**Errors:** `400` invalid parameters, `404` execution not found, `410` requested history is no longer retained, `429` [too many requests in progress](#http-429--too-many-requests-in-progress)

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

This is the one sandbox route that never wakes a stopped sandbox. An execution
lives only in the guest's memory, so a sandbox that is stopped, or that was
restarted after a crash, has already lost it and the runner answers `204` without
starting anything. It therefore never returns `409 sandbox_restarted` either, and
never spends the one restart report a crash gets — that report is kept for the next
request a client actually reads. The SDK relies on this: it deletes an execution in
the background after every command and discards the answer.

**Path Parameters:**
- `id` — Sandbox UUID
- `exec_id` — Execution ID (from the `started` event)

**Response:** `204 No Content`

**Errors:** `400` invalid id, `404` sandbox not found, `429` past this route's own limits (see [HTTP 429](#http-429--too-many-requests-in-progress)), `503` if the sandbox does not answer within 10 seconds

**Example:**

```bash
curl -X DELETE http://localhost:8080/sandboxes/550e8400-e29b-41d4-a716-446655440000/executions/a1b2c3d4-... \
  -H "X-Api-Key: YOUR_API_KEY"
```

---

### GET /sandboxes/{id}/files

List files in a sandbox directory.

File paths are absolute: a `path` is taken from `/`, so `notes.txt` is `/notes.txt`, not relative to an execution's working directory. A path the sandbox does not permit reading or writing is `403` on every file endpoint below; a recursive list skips entries under it that it cannot read.

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

**Errors:** `400` invalid id, `404` directory not found, `429` [too many requests in progress](#http-429--too-many-requests-in-progress)

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

**Errors:** `400` invalid id or missing path, `404` file not found, `429` [too many requests in progress](#http-429--too-many-requests-in-progress)

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

**Errors:** `400` invalid id or missing path, `409` file exists (when `overwrite=false`), `429` [too many requests in progress](#http-429--too-many-requests-in-progress)

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

**Errors:** `400` invalid id or missing path, `404` path not found, `429` [too many requests in progress](#http-429--too-many-requests-in-progress)

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

**Errors:** `400` invalid id or missing path, `429` [too many requests in progress](#http-429--too-many-requests-in-progress)

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

**Errors:** `400` invalid id, missing src/dest, `404` source not found, `409` destination exists, `429` [too many requests in progress](#http-429--too-many-requests-in-progress)

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

**Errors:** `400` invalid id, missing src/dest, `404` source not found, `409` destination exists, `429` [too many requests in progress](#http-429--too-many-requests-in-progress)

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

**Errors:** `400` invalid id or missing path, `409` directory already exists, `429` [too many requests in progress](#http-429--too-many-requests-in-progress)

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

**Errors:** `400` invalid id or missing path, `404` file not found, `429` [too many requests in progress](#http-429--too-many-requests-in-progress)

**Example:**

```bash
curl "http://localhost:8080/sandboxes/550e8400-e29b-41d4-a716-446655440000/stat?path=/home/user/file.txt" \
  -H "X-Api-Key: YOUR_API_KEY"
```

---

## Admin: tenants and API keys

All `/admin/*` routes require an admin API key (`SANDBOX_API_KEYS`) unless marked "Admin or provisioner" or "Admin or scaler". Any other key receives `403`.

### GET /admin/tenants

Admin only. List tenants.

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

Admin or provisioner. Create a tenant. By default also mints one API key (`create_key` defaults to `true`).

**Request body (optional):**

```json
{
  "name": "my-instance",
  "external_ref": "n8n-instance-id",
  "max_sandboxes": 50,
  "create_key": true
}
```

| Field          | Type   | Required | Default |
|----------------|--------|----------|---------|
| `name`         | string | no       | `""` |
| `external_ref` | string | no       | `""` |
| `max_sandboxes`| int    | no       | `SANDBOX_API_DEFAULT_MAX_SANDBOXES` (`50`) |
| `create_key`   | bool   | no       | `true` |
| `max_idle_window` | int (seconds) | no | none: `SANDBOX_API_MAX_IDLE_WINDOW` |

`name` is an optional human-readable label. `external_ref` is an optional opaque id from the caller (for example an n8n instance id); the API does not enforce uniqueness.

`max_sandboxes` is the per-tenant sandbox quota (`0` = unlimited). Must be between `0` and `2147483647`. When omitted, the service default applies. With a provisioner key an explicit value must be between `1` and `SANDBOX_API_DEFAULT_MAX_SANDBOXES`; `0` and larger values return `400`.

`max_idle_window` caps the per-sandbox idle windows of the tenant's sandboxes, from 60 up to `SANDBOX_API_MAX_IDLE_WINDOW`; it narrows the server's cap, never widens it. The tenant body includes it only when set.

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

Admin only. Get a tenant by id.

### PATCH /admin/tenants/{id}

Admin or provisioner. Change a tenant's limits. A provisioner key may change any tenant, not only the ones it created.

**Request body:**

```json
{
  "max_sandboxes": 20
}
```

At least one of `max_sandboxes` and `max_idle_window` is required, each with the bounds of [create](#post-admintenants): `max_sandboxes` `0`…`2147483647` with an admin key, `1`…`SANDBOX_API_DEFAULT_MAX_SANDBOXES` with a provisioner key. `"max_idle_window": null` removes the tenant's cap. Unknown fields return `400`.

The tenant's sandboxes are never touched, and keep the idle windows they have. If the new limit is below the tenant's current count, creating a new sandbox returns `403` until the tenant is below it again.

**Response:** `200 OK` with the updated tenant, as `GET /admin/tenants/{id}` returns it.

```json
{
  "id": "uuid",
  "name": "my-instance",
  "external_ref": "n8n-instance-id",
  "max_sandboxes": 20,
  "created_at": 1700000000
}
```

**Errors:** `400` invalid tenant id, invalid body, or `max_sandboxes` missing or out of bounds, `403` tenant key, `404` tenant not found

**Example:**

```bash
curl -X PATCH http://localhost:8080/admin/tenants/0f0e0d0c-0000-4000-8000-000000000001 \
  -H "X-Api-Key: PROVISIONER_KEY" \
  -H "Content-Type: application/json" \
  -d '{"max_sandboxes":20}'
```

### DELETE /admin/tenants/{id}

Admin or provisioner. Delete a tenant and its API keys (`204`).

Fails with `409 Conflict` if the tenant still owns sandboxes — delete those first with an admin key or the tenant's own key; a provisioner key cannot. This avoids orphaning running sandboxes after credentials are removed.

`DeleteTenant` locks the tenant row while checking ownership and deleting, and tenant sandbox `Create` takes the same lock before insert. That closes the race where a sandbox create is in flight (runner VM already started, store row not yet written) while delete sees an empty count.

### GET /admin/tenants/{id}/keys

Admin only. List API key metadata for a tenant (no plaintext secrets).

### POST /admin/tenants/{id}/keys

Admin only. Mint an additional API key for the tenant. Returns plaintext `api_key` once (`201`).

### DELETE /admin/tenants/{id}/keys/{keyId}

Admin only. Revoke an API key (`204`). Revoked keys are rejected from the next request on.

---

## Admin: runners

### GET /admin/runners

Admin or scaler. Lists every registered runner, ordered by `id`, with the capacity it last reported. Runners that stopped heartbeating stay in the list: the in-memory registry drops a runner when it disconnects, the Postgres registry never does. Filter on `healthy` and `last_seen` to keep the live ones. Runner addresses are not returned.

**Response:** `200 OK`

```json
{
  "runners": [
    {
      "id": "runner-1",
      "healthy": true,
      "capacity_total": 20,
      "capacity_used": 7,
      "capacity_stopped": 2,
      "cordoned": false,
      "templates": ["default"],
      "last_seen": 1700000000
    }
  ]
}
```

`cordoned` is true while the runner is [draining](#get-post-delete-adminrunnersiddrain). `templates` lists what it serves. `last_seen` is the Unix time in seconds of the runner's last heartbeat. `runners` is `[]` when none are registered.

**Errors:** `403` provisioner or tenant key, `500` registry read failed

**Example:**

```bash
curl http://localhost:8080/admin/runners -H "X-Api-Key: SCALER_KEY"
```

---

### GET, POST, DELETE /admin/runners/{id}/drain

Admin only. `POST` starts draining the runner: it is cordoned, so it refuses creates and placement skips it, while its sandboxes keep serving, wakes included. `DELETE` stops draining. `GET` reports the state. Each answers with it:

```json
{
  "runner_id": "runner-1",
  "draining": true,
  "drained": false,
  "active_executions": 1,
  "open_streams": 1,
  "capacity_used": 7,
  "capacity_stopped": 2
}
```

`POST` may send `{"archive": true}`: this API pod then archives every non-ephemeral sandbox on the runner as it goes idle (stopping running ones), and `drained` also waits until only ephemeral sandboxes are left there, so an upgrade keeps every sandbox's files. The response says `"archive": true` while it runs; `DELETE` ends it.

`drained` is true once the runner is draining and has no running execution and no open execution stream (see [activity](#get-sandboxesid)), so it can be restarted without cutting a run short. A restart still deletes every sandbox the runner hosts. The drain lives in the runner's memory: a restarted runner comes back undrained. `capacity_used` and `capacity_stopped` are as in [`GET /admin/runners`](#get-adminrunners).

**Errors:** `403` any key but admin, `404` runner not registered, `503` the runner did not answer (with `Retry-After`)

---

## Admin: capacity scaler

### GET /admin/scaler

Admin only. Forwards to `SANDBOX_API_SCALER_URL`'s `/policy` endpoint; on success, relays the response body unchanged and copies `Content-Type` when present. The scaler itself is operated privately, outside this repository.

The scaler authenticates its own callers, so send its bearer token in the `Authorization` header; the API forwards that header as is. The admin key in `X-Api-Key` is not forwarded.

- `SANDBOX_API_SCALER_URL` unset → `503 {"error": "scaler not configured", "code": 503}`.
- Set but the scaler is down, times out (5s), redirects, answers anything other than `200` (including `401` for a missing or wrong token), or returns a body over 1 MiB → `503 {"error": "scaler unavailable", "code": 503}`.
- Set and reachable → `200` with the scaler's response body, unchanged.
