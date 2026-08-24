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
- `tracestate` and vendor extensions are not propagated.
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
| `op` | `create` or `ensure_running`, matching the metric labels |
| `total_ms` | Whole operation |
| `<step>_ms` | One field per step below |

Steps, in order:

| Step | Operation | What it covers |
| --- | --- | --- |
| `clone_rootfs` | create | Copy the template rootfs for the new sandbox |
| `clone_snapshot` | create | Copy the golden memory and state snapshot |
| `prepare_jail` | create, wake | Jailer directory and device setup |
| `setup_network` | create, wake | Network namespace, tap device, and rules |
| `start_jailer` | create, wake | Launch the jailer and Firecracker process |
| `wait_socket` | create, wake | Wait for the Firecracker API socket |
| `load_snapshot` | create, wake | Restore the VM from the snapshot |
| `start_proxy` | create, wake | Start the host-side daemon proxy |
| `probe_daemon` | create, wake | First successful call to the in-guest daemon |

A wake skips the two clone steps, since the sandbox's disk already exists.

### Runner: guest deaths

`firecracker guest died` is emitted once when a microVM's process exits without
the runner having killed it — a guest kernel panic, a host `SIGKILL` of the VMM,
or a reboot from inside the guest. It has no `trace_id`: nothing requested it.

| Field | Notes |
| --- | --- |
| `sandbox_id`, `vm_id`, `slot` | Identity of the sandbox and the slot it was on |
| `err` | The wait error of the exited process, or null on a clean exit |

The runner then tears the dead microVM down and hands its slot back, so the
sandbox goes on to report as stopped with its files intact. It is not restored
from its snapshot afterwards: the guest kept writing to the rootfs past the point
that snapshot was taken. A request for it fails, and the sandbox stays deletable.

## Metrics

The step timings also go to `sandbox_lifecycle_step_duration_seconds{operation,step}`
on the runner, for percentiles across the fleet. Use the events to take apart a
single slow operation, the histogram to see whether it is representative.

Related series:

- `sandbox_guest_deaths_total` — one increment per `firecracker guest died`
  event, for alerting on a crash rate the events alone would not surface.
- `sandbox_container_operation_duration_seconds{operation}` — the totals the
  steps add up to.
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
