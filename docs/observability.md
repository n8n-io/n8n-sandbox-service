# Observability

The service answers "why was this request slow?" with two things: a trace id
that runs through every log event a request touches, and events wide enough that
one line explains one operation. Metrics stay for aggregates and alerting.

## Trace context

The API establishes a [W3C trace context](https://www.w3.org/TR/trace-context/)
for every request in its outermost middleware, then forwards it to the runner:

| Hop | Carrier |
| --- | --- |
| Client → API | `traceparent` HTTP header (optional, see [API.md](API.md#request-tracing)) |
| API → runner (exec, files) | `traceparent` HTTP header on the proxied request |
| API → runner (create, stop, delete) | `traceparent` gRPC metadata |
| Runner → sandbox lifecycle | Go context, down to the create and wake events |

Rules:

- An inbound `traceparent` is adopted only if it is a valid W3C value, and only
  its trace id, parent id, and flags are kept; anything else is replaced with a
  freshly generated one. What the API logs and forwards to the runner is always
  rebuilt from those parsed fields, so a caller cannot inject content into the
  service's logs or make it relay bytes of their choosing.
- `tracestate` is never read or logged. The HTTP hops forward it unchanged, like
  any other header; the gRPC hop does not carry it.
- Every event carries `trace_id`, the 32 hex character trace-id field of the
  traceparent. That is the join key across processes.
- A wake started by one request may be shared by others that arrive while it
  runs, so the wake event carries the trace id of the request that started it.

## Events

Four log messages are canonical events: one line that explains one operation,
and what the queries below select on.

| Event | Emitter | Emitted when |
| --- | --- | --- |
| `request` | API | A request finishes, including one rejected by middleware |
| `request` | Runner | A request proxied from the API (exec, files) finishes |
| `firecracker sandbox created` | Runner | A sandbox is created and its guest daemon answers |
| `firecracker sandbox woke` | Runner | A stopped sandbox is restored and its guest daemon answers |
| `firecracker guest died` | Runner | A microVM exited without the runner stopping it |
| `docker guest died` | Runner | A sandbox container exited without the runner stopping it |
| `docker sandbox recovered` | Runner | A restarted container's network policy was reapplied and its daemon answered |

The runner's `request` event only records `trace_id`, `method`, `path`, and
`duration_ms`; the API's is the wide one, and the runner's lifecycle events hold
the detail of what happened underneath. Health and metrics routes are logged on
neither binary.

Diagnostics are the other lines the two binaries log. They are not part of this
contract — no guarantee of one per operation, and their fields change with the
code that emits them — but a diagnostic logged with a request's context also
carries that request's `trace_id`, so the step a failed create died on comes
back alongside its canonical event. The create path does this for runner
selection, the gRPC create, and the store write.

To get it in new code, log through `slog`'s context-taking functions
(`InfoContext`, `ErrorContext`, and so on): the handler both binaries install
adds the trace id whenever the context has one. Background work such as the
idle sweeper has no request context and so no trace id.

### API: one event per request

The API emits exactly one `request` event per request, whatever the outcome:

| Field | Notes |
| --- | --- |
| `trace_id` | Join key for every other event of this request |
| `method`, `path`, `query`, `status`, `duration_ms` | Always present |
| `tenant_id` | Present for tenant-authenticated requests |
| `sandbox_id`, `runner_id` | Present on create and on proxied sandbox routes |
| `ttfb_ms` | Proxied routes: time until the runner's response headers arrived. For a streamed exec this is time to first byte, not the full response |

### Runner: one event per sandbox lifecycle operation

The Firecracker runner emits `firecracker sandbox created` and
`firecracker sandbox woke` with the full step breakdown:

| Field | Notes |
| --- | --- |
| `trace_id` | Matches the API event for the request that caused the operation |
| `sandbox_id`, `vm_id`, `slot` | Identity of the sandbox and the slot it occupies |
| `op` | `create`, `ensure_running` or `recover`, matching the metric labels |
| `recovered` | On a wake: whether it was a crash recovery rather than an ordinary wake |
| `total_ms` | Whole operation |
| `<step>_ms` | One field per step below |

Steps, in order:

| Step | Operation | What it covers |
| --- | --- | --- |
| `clone_rootfs` | create | Copy the template rootfs for the new sandbox |
| `clone_snapshot` | create | Copy the golden memory and state snapshot |
| `prepare_jail` | create, wake, recover | Jailer directory and device setup |
| `setup_network` | create, wake, recover | Network namespace, tap device, and rules |
| `start_jailer` | create, wake, recover | Launch the jailer and Firecracker process |
| `wait_socket` | create, wake, recover | Wait for the Firecracker API socket |
| `load_snapshot` | create, wake | Restore the VM from the snapshot |
| `cold_boot` | recover | Boot the sandbox's own rootfs, replacing the restore |
| `start_proxy` | create, wake, recover | Start the host-side daemon proxy |
| `probe_daemon` | create, wake, recover | First successful call to the in-guest daemon |

A wake skips the two clone steps, since the sandbox's disk already exists. A
recovery skips them too and swaps `load_snapshot` for `cold_boot` — the field to
compare when reading recovery latency against an ordinary wake.

### Runner: guest deaths

`firecracker guest died` is emitted once when a microVM's process exits without
the runner having killed it — a guest kernel panic, a host `SIGKILL` of the VMM,
or a reboot from inside the guest. It has no `trace_id`: nothing requested it.

| Field | Notes |
| --- | --- |
| `sandbox_id`, `vm_id`, `slot` | Identity of the sandbox and the slot it was on |
| `err` | The wait error of the exited process, or null on a clean exit |

The runner then tears the dead microVM down and hands its slot back, so the
sandbox goes on to report as stopped with its files intact. Nothing is recovered
until a request arrives; that request cold boots the sandbox's own rootfs — never
its snapshot, which the guest wrote past — and is then failed with
`409 sandbox_restarted`. The recovery is the `firecracker sandbox woke` event with
`op=recover`.

On the Docker runner the same crash reads differently, because Docker's restart
policy has already brought the container back:

| Field | Notes |
| --- | --- |
| `sandbox_id`, `container_id` | Identity of the sandbox and the container that died |

`docker guest died` comes from the runner's `docker events` stream rather than
from a process it waits on, so a `docker event stream ended, reconnecting`
warning is worth alerting on: while it is down, crashes go unreported and
restarted sandboxes are served without their `409`. Neither gauge moves for a
Docker crash — the container never leaves — so the death counter is the only
signal. The repair is `docker sandbox recovered`, emitted once the restarted
container's network policy has been reapplied for the address it came back on.

## Metrics

The step timings also go to `sandbox_lifecycle_step_duration_seconds{operation,step}`
on the runner, for percentiles across the fleet. Use the events to take apart a
single slow operation, the histogram to see whether it is representative.

Related series:

- `sandbox_guest_deaths_total` — one increment per guest death event on either
  backend, for alerting on a crash rate the events alone would not surface.
- `sandbox_recoveries_total{result}` — recovery attempts, one per crash rather
  than one per request the crash stranded. Read against the death counter: deaths
  without recoveries are crashed sandboxes nobody came back to, and
  `result="error"` is one the runner could not bring back, including a sandbox
  refused for want of capacity before any boot was attempted. `result` carries
  `success` or `error`, the same pair as every other `result` label here.
- `sandbox_container_operation_duration_seconds{operation}` — the totals the
  steps add up to, a recovery's under `operation="recover"` like its steps.
- `sandbox_http_request_duration_seconds{role,route,method}` — end-to-end per
  route on both binaries.

Full list in [configuration.md](configuration.md#metrics).

## Working with the events

Both binaries log JSON to stdout, so the examples below work on any log stream —
`kubectl logs`, `journalctl -o cat`, or a log file from the e2e scripts.

A runner's journal is not pure JSON: the runtime shells out through `sudo` for
jail and network setup, and those lines land in the same unit. Select the events
you want with `grep` before handing anything to `jq`.

Everything one request touched, across both processes:

```sh
cat api.log runner.log | jq 'select(.trace_id == "4bf92f3577b34da6a3ce929d0e0e4736")'
```

The ten slowest wakes, with their step breakdown:

```sh
grep -a '"firecracker sandbox woke"' runner.log \
  | jq -s 'sort_by(-.total_ms) | .[:10]'
```

Percentiles for every step of an operation, which is what a baseline entry
needs. This runs on a stream of any size and prints a summary small enough to
come back through `az vmss run-command`:

```sh
grep -a '"firecracker sandbox created"' runner.log | jq -s '
  . as $e
  | (map(keys[] | select(endswith("_ms"))) | unique) as $steps
  | {n: length,
     steps: [$steps[] | . as $s
       | {key: $s, value: (($e | map(.[$s]) | sort) as $v
           | {p50: $v[(($v|length-1) * 0.5) | floor],
              p95: $v[(($v|length-1) * 0.95) | round],
              max: ($v | max)})}] | from_entries}'
```
