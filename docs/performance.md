# Performance baseline

Recorded numbers for the sandbox service. Each entry states what was measured,
on which runtime, on what host shape, and at which commit.

Measurements are only comparable within the same runtime, host shape and commit.
When any of them changes, record a new entry rather than editing an old one.

## How to record a baseline

1. Deploy the commit under test and note the API and runner image tags.
2. Run the k6 baseline scenario from inside the cluster, so client-side numbers
   are not dominated by the trip to the ingress:

   ```sh
   kubectl run k6-baseline --rm -i --restart=Never --image=grafana/k6:latest \
     --env BASE_URL=http://<api service>:8080 \
     --env API_KEY=<key> \
     --env SCENARIO=baseline \
     --env ITERATIONS=30 \
     -- run - < benchmarks/k6-sandbox-lifecycle.js
   ```

3. Measure wakes as a separate, shorter run. Each iteration has to idle long
   enough for the sweeper to stop the sandbox, so a wake run costs roughly
   `WAKE_AFTER` seconds per iteration and needs far fewer of them:

   ```sh
   kubectl run k6-wake --rm -i --restart=Never --image=grafana/k6:latest \
     --env BASE_URL=http://<api service>:8080 \
     --env API_KEY=<key> \
     --env SCENARIO=baseline \
     --env ITERATIONS=10 \
     --env WAKE_AFTER=<idle stop + sweep interval, plus margin> \
     -- run - < benchmarks/k6-sandbox-lifecycle.js
   ```

   Sizing `WAKE_AFTER` and checking that the wakes really happened is covered in
   [benchmarks/README.md](../benchmarks/README.md#measuring-wake).
4. On the Firecracker runtime, take the server-side split from the runner's
   lifecycle events. No other runtime emits step timings, so entries for those
   are client-side only. Select the events by message before parsing, because a
   runner's journal interleaves non-JSON lines from the `sudo` calls the runtime
   makes:

   ```sh
   grep -a '"firecracker sandbox created"' runner.log | jq -c .
   ```

   Field meanings are in [observability.md](observability.md#events).
5. Add an entry below with the runtime, the host shape, the commit, and the
   numbers.

Keep environment identifiers — cluster, scale set, namespace, host names — out
of the entries. Record the host shape and the configuration that affects the
result, not where it ran.

## Entries

### 2026-08-18 — staging, Firecracker

| Field | Value |
| --- | --- |
| Commit | `94afb97` |
| Runner host | Azure `Standard_D4s_v3`, 4 vCPU, 16 GB, 2 instances |
| Scenario | create `ITERATIONS=30`, wake `ITERATIONS=10 WAKE_AFTER=60`, 1 VU, k6 in-cluster |
| Idle settings during run | `idleStopAfter: 30s`, `idleSweepInterval: 15s` |

Client-side, from k6:

| Metric | p50 | p95 |
| --- | --- | --- |
| `sandbox_create_duration` | 732 ms | 762 ms |
| `sandbox_exec_duration` | 82 ms | 90 ms |
| `sandbox_wake_exec_duration` | 240 ms | 252 ms |
| `sandbox_delete_duration` | 112 ms | 121 ms |

Server-side, from the runner's create (n=58) and wake (n=23) events. Ranges span
the two runner instances:

| Step | create p50 | create p95 | wake p50 | wake p95 |
| --- | --- | --- | --- | --- |
| `clone_rootfs` | 396–400 ms | 413–414 ms | n/a | n/a |
| `clone_snapshot` | 118–120 ms | 122–132 ms | n/a | n/a |
| `prepare_jail` | 26 ms | 39–41 ms | 28–30 ms | 37 ms |
| `setup_network` | 85–89 ms | 103–105 ms | 89–92 ms | 96–105 ms |
| `start_jailer` | 0 ms | 0 ms | 0 ms | 0 ms |
| `wait_socket` | 20 ms | 21–40 ms | 20 ms | 20–21 ms |
| `load_snapshot` | 3 ms | 5 ms | 3–4 ms | 4–7 ms |
| `start_proxy` | 0 ms | 0 ms | 0 ms | 0 ms |
| `probe_daemon` | 56–61 ms | 65–66 ms | 36–37 ms | 39–42 ms |
| `total_ms` | 713–726 ms | 768–773 ms | 183–185 ms | 192–208 ms |

Conditions:

- `http_req_failed` was 0% on both k6 runs.
- The runner data directory was on the OS disk, formatted ext4, and the golden
  template files were resident in page cache throughout. `clone_rootfs` and
  `clone_snapshot` are copy-through-cache times under those conditions.
- The event window covered more traffic than the k6 runs alone: 58 creates and
  23 wakes against 40 and 10 driven by k6.
- Client-side create p50 exceeded runner-side create p50 by 6 ms.
- Minimum `sandbox_wake_exec_duration` was 229 ms against a warm
  `sandbox_exec_duration` p50 of 82 ms, so every wake iteration hit a stopped
  sandbox.

---

Template for further entries:

### YYYY-MM-DD — `<environment class>`, `<runtime>`

| Field | Value |
| --- | --- |
| Commit | `<sha>` |
| Runner host | e.g. Azure `Standard_D8ds_v5`, 8 vCPU, 32 GB, local NVMe |
| Scenario | e.g. create `ITERATIONS=30`, wake `ITERATIONS=10 WAKE_AFTER=60` |
| Idle settings during run | `idleStopAfter`, `idleSweepInterval` |

Client-side, from k6:

| Metric | p50 | p95 |
| --- | --- | --- |
| `sandbox_create_duration` | | |
| `sandbox_exec_duration` | | |
| `sandbox_wake_exec_duration` | | |
| `sandbox_delete_duration` | | |

Server-side, from the runner's create and wake events. Firecracker only:

| Step | create p50 | create p95 | wake p50 | wake p95 |
| --- | --- | --- | --- | --- |
| `clone_rootfs` | | | n/a | n/a |
| `clone_snapshot` | | | n/a | n/a |
| `prepare_jail` | | | | |
| `setup_network` | | | | |
| `start_jailer` | | | | |
| `wait_socket` | | | | |
| `load_snapshot` | | | | |
| `start_proxy` | | | | |
| `probe_daemon` | | | | |
| `total_ms` | | | | |

Conditions: sample counts, error rates, and any configuration set for the run.
