# Performance baseline

Recorded numbers for the Firecracker runner, so later changes can be judged
against something instead of against memory. Each entry states what was
measured, on what, and how to reproduce it.

Measurements are only comparable within the same host shape and commit. When
either changes, record a new entry rather than editing an old one.

## How to record a baseline

1. Deploy the commit under test and note the API and runner image tags.
2. Run the k6 baseline scenario from inside the cluster, so client-side numbers
   are not dominated by the trip to the ingress:

   ```sh
   kubectl run k6-baseline --rm -i --restart=Never --image=grafana/k6:latest \
     --env BASE_URL=http://n8n-sandbox-service-api:8080 \
     --env API_KEY=<key> \
     --env SCENARIO=baseline \
     --env ITERATIONS=30 \
     --env WAKE_AFTER=8 \
     -- run - < benchmarks/k6-sandbox-lifecycle.js
   ```

   `WAKE_AFTER` only produces wakes if the API's idle sweeper stops the sandbox
   in time; see [benchmarks/README.md](../benchmarks/README.md#measuring-wake).
3. Take the server-side split from the runner's lifecycle events, which is the
   part that a change to the runtime actually moves:

   ```sh
   jq -c 'select(.msg == "firecracker sandbox created" or .msg == "firecracker sandbox woke")' runner.log
   ```

   Field meanings are in [observability.md](observability.md#events).
4. Add an entry below with the host shape, the commit, and the numbers.

## Entries

None recorded yet. The instrumentation this depends on landed in the same change
as this document; the first entry follows the next stage deploy.

Use this shape:

### YYYY-MM-DD — stage, Firecracker

| Field | Value |
|-------|-------|
| Commit | `<sha>` |
| Runner host | e.g. Azure `Standard_D8ds_v5`, 8 vCPU, 32 GB, local NVMe |
| Sandboxes on host during run | e.g. 0 other sandboxes |
| Scenario | `SCENARIO=baseline ITERATIONS=30 WAKE_AFTER=8` |

Client-side, from k6:

| Metric | p50 | p95 | max |
|--------|-----|-----|-----|
| `sandbox_create_duration` | | | |
| `sandbox_exec_duration` | | | |
| `sandbox_wake_exec_duration` | | | |
| `sandbox_delete_duration` | | | |

Server-side, from the runner's create and wake events:

| Step | create p50 | wake p50 |
|------|-----------|----------|
| `clone_rootfs` | | n/a |
| `clone_snapshot` | | n/a |
| `prepare_jail` | | |
| `setup_network` | | |
| `start_jailer` | | |
| `wait_socket` | | |
| `load_snapshot` | | |
| `start_proxy` | | |
| `probe_daemon` | | |
| `total_ms` | | |

Notes: anything unusual about the run — errors, retries, noisy neighbours, or
capacity limits hit.
