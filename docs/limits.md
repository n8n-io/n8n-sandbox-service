# Limits

Every limit a client or operator can run into, with its default. The variables are described in [configuration.md](configuration.md), the status codes in [API.md](API.md).

## Requests in progress

| Limit | Default | Set by | When reached |
| --- | --- | --- | --- |
| Requests in progress per tenant, on each API replica | 512 | `SANDBOX_API_MAX_INFLIGHT_PER_TENANT` | `429` `tenant_request_limit`, `Retry-After: 1` |
| Execution `DELETE`s in progress per tenant, on each API replica | 16 | fixed | the same |
| Requests in progress per sandbox, from all replicas | 256 | `SANDBOX_RUNNER_MAX_INFLIGHT_PER_SANDBOX` | `429` `sandbox_request_limit`, `Retry-After: 1` |
| Execution `DELETE`s in progress per sandbox | 4 | fixed | the same |

Creates, exec and file requests count; a stream counts until it ends, a create until the runner has finished it. `GET /sandboxes`, `GET` and `DELETE /sandboxes/{id}`, admin routes and admin keys are not counted. Setting a limit to `0` turns it and its `DELETE` reserve off. Nothing caps all tenants together on a replica.

## Request rate

There is no rate limit, and failed authentication is not throttled. Each request with a tenant key costs a database lookup, a wrong key too.

## Sandboxes

| Limit | Default | Set by | When reached |
| --- | --- | --- | --- |
| Sandboxes per tenant | 50 | the tenant's `max_sandboxes`: `SANDBOX_API_DEFAULT_MAX_SANDBOXES` when created; admin keys may set any value, `0` = unlimited; provisioner keys 1 to the default | `403` `tenant sandbox quota exceeded`. Lowering it keeps existing sandboxes |
| Sandboxes per runner | 1000 | `SANDBOX_RUNNER_CAPACITY_TOTAL` | the API tries up to 3 runners within 5 s, then `503` `no_capacity`, `Retry-After: 1` |
| Free space on a Firecracker runner's data volume | 5%, plus the volume cache's unused budget when the cache is on that volume | fixed; `SANDBOX_RUNNER_VOLUME_CACHE_MAX_BYTES` | creates and volume builds are refused like a full runner |

Sandboxes created with an admin key count against no quota. The API knows a runner's usage from its last heartbeat (every 10 s), so a burst of creates can overshoot a Docker runner, which counts stopped sandboxes too and never refuses. A Firecracker runner refuses past its slots, and the API tries another. On Firecracker a stopped sandbox gives up its slot, so waking it needs a free one, otherwise `503` with no `Retry-After`; a stop with no room for its memory snapshot fails and the sandbox keeps running ([data volume](../internal/runner/runtime/firecracker.ee/README.md#data-volume)).

## Sandbox lifetime

A sandbox has no maximum lifetime; it lives as long as it is used.

| Limit | Default | Set by |
| --- | --- | --- |
| Idle time before a sandbox is stopped, or deleted if `ephemeral` | 1 h | `SANDBOX_API_IDLE_STOP_AFTER` |
| Idle time before a stopped sandbox is deleted; requests get `404` from then on | 24 h | `SANDBOX_API_IDLE_DELETE_AFTER` |
| Extra wait before a delete | 1 min | `SANDBOX_API_IDLE_DELETE_SAFETY_BUFFER` |
| How often the sweeper checks | 1 min | `SANDBOX_API_IDLE_SWEEP_INTERVAL` |

Exec and file requests the sandbox served count as use, `GET /sandboxes/{id}` does not ([details](API.md#get-sandboxesid)). Use is recorded when a response starts, so a command that runs longer than the idle stop window can be stopped mid-run.

## Executions

| Limit | Default | Set by | When reached |
| --- | --- | --- | --- |
| Run time of a command | 5 min | `timeout_ms` | the command is killed; the `exit` event has `timed_out: true` |
| Largest `timeout_ms` | 15 min | `SANDBOX_RUNNER_MAX_EXEC_TIMEOUT`, `0` = no limit | `400` |
| Commands per sandbox | no limit | | a command outlives its stream, so the requests in progress do not cap them; until they exit or time out they share the sandbox's processes and memory, which also holds their kept output |
| Output kept per execution | 16 MiB | `SANDBOX_EXEC_MAX_EVENT_BYTES` | the oldest events are dropped; asking for them gets `410`, and a stream that falls that far behind ends without `exit` |
| How long a finished execution is kept | 10 min, checked every minute | `SANDBOX_EXEC_RETAIN` | `GET` answers `404`; a `POST` with the same `exec_id` runs the command again |
| Cancelling with `DELETE` | 10 s | fixed | `503` |

Output arrives in pieces of at most 64 KiB; there is no limit on total output.

## Sizes

| Limit | Default | Set by | When reached |
| --- | --- | --- | --- |
| File upload, per request | 10 MiB | `SANDBOX_API_MAX_FILE_BYTES` and `SANDBOX_RUNNER_MAX_FILE_BYTES`; the lower wins | `400` `request body too large`. Appending can grow a file past it |
| File download | no limit | | |
| File listing, copy, move | no limit | | |
| `GET /sandboxes` | no paging | | |
| Body of a create | 1 KiB | fixed | `400` |
| Body of a tenant create or update | 4 KiB | fixed | `400` |
| Body of an exec, copy or move | 1 MiB | fixed | `400` |

## Time limits on requests

| What | Limit |
| --- | --- |
| Sending the request headers (API, runner and daemon) | 10 s |
| Sending the whole request, so also an upload | 30 s |
| An idle keep-alive connection | 120 s |
| A response, including a stream | no limit; 30 s on the API's dedicated metrics listener |
| Creating a sandbox | 3 min on the runner; the API waits up to 4 min and finishes the create even if the client has gone |
| Stopping, waking or deleting a sandbox on the runner | 2 min; the sweeper waits up to 3 min |
| A started or woken sandbox's daemon to answer | 60 s |
| `PUT`/`DELETE /admin/runners/{id}/cordon`, `GET /admin/scaler` | 10 s, 5 s |

The API sets no deadline on its request-path database calls or on a runner's response headers, so a database or runner that hangs holds requests until their clients give up.

Each API replica uses at most 25 database connections, 1 with SQLite, and a request waits for a free one. With Postgres it opens up to `SANDBOX_API_IDLE_SWEEP_CONCURRENCY` + 5 more (13 by default) for sandbox locks. A create that names an `id` and a `DELETE /sandboxes/{id}` each hold one until they finish, and a sweep holds up to `SANDBOX_API_IDLE_SWEEP_CONCURRENCY`, leaving 5 for requests on the sweeping replica; further ones wait. Size Postgres `max_connections` for both pools on every replica.

## Resources per sandbox

| Resource | Docker (Sysbox) | Firecracker |
| --- | --- | --- |
| Memory | 512 MB, `SANDBOX_RUNNER_DEFAULT_MEMORY_MB` | 512 MiB, fixed when the golden snapshot is built |
| CPU | 1 core, `SANDBOX_RUNNER_DEFAULT_CPU_PERCENT` | 1 vCPU, fixed when the golden snapshot is built |
| Processes | 256, `SANDBOX_RUNNER_DEFAULT_PIDS_MAX` | no limit set |
| Disk | no quota, unless `SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB` and a [quota pool](configuration.md#disk-quotas) | 2 GiB root filesystem, fixed when the template is built |
| Network | egress `public` or `none`, chosen at create; no bandwidth limit | the same |

On Docker, memory, CPU and process limits apply only with `SANDBOX_RUNNER_ENABLE_CGROUPS=true` (the default).

## Retries

`429` and `503` `no_capacity` carry `Retry-After: 1`; other `503`s carry none. The [SDK](../sdk/README.md) does not read `Retry-After`. It retries `429`; `503` and network errors only for idempotent requests; and a create on `503` `no_capacity`, or on any `503` when it names an `id`. It retries up to 3 times after 200 ms, doubling up to 10 s, with jitter, and resumes a broken exec stream up to 10 times per command, 250 ms apart.
