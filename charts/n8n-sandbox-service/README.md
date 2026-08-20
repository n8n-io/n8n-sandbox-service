# n8n Sandbox Service Helm Chart

This chart deploys the n8n Sandbox Service API and, optionally, an in-cluster Docker-in-Docker runner.

## Install

Create or provide:

- An auth Secret with API keys and runner registration tokens, or set `auth.*` values and let the chart create one.
- TLS Secrets for the API registration server, API control client, runner registration client, and runner control server, or set `tls.mode=certManager`.
- A Sysbox-ready node pool for `runner.isolation=sysbox` (the default). Use `runner.isolation=privileged` on clusters that cannot install sysbox.

```sh
helm upgrade --install n8n-sandbox-service ./charts/n8n-sandbox-service \
  --namespace n8n-sandbox \
  --create-namespace
```

## Data Plane Mode

`dataPlane.mode` selects where the data plane runs:

- `in-cluster` — the chart renders a Docker-in-Docker runner StatefulSet. `runner.isolation` selects how the runner gets its privileges.
- `external` — runners live outside Kubernetes. The chart renders only the API resources.

The Firecracker runner is a separate image/entrypoint for external host deployments and is not charted here yet.

After install, if no runner pod appears, inspect the StatefulSet events. RuntimeClass and Pod Security Admission rejections land there, not on a pod:

```sh
kubectl -n n8n-sandbox describe statefulset -l app.kubernetes.io/name=n8n-sandbox-service
```

## Runner Isolation

`runner.isolation` selects the source of privilege for the in-cluster runner. Both isolations run the same runner image.

- `sysbox` (default) — run under the `sysbox-runc` RuntimeClass. Needs sysbox installed on the nodes. Recommended.
- `privileged` — run with `privileged: true`. Any node can run it. See below.

The isolation-specific settings (`runtime` and `scheduling`) live in the `runner.sysbox` and `runner.privileged` subblocks. The chart reads only the subblock that matches `runner.isolation`; values in the other subblock are ignored. All other `runner.*` keys are shared between isolations.

The runner resource names follow the isolation (`<release>-sysbox-runner`, `<release>-privileged-runner`). Switching isolation therefore recreates the runner StatefulSet under the other name; this is required because StatefulSet selectors are immutable.

The runner name caps at 61 characters. The StatefulSet appends `-<ordinal>` to build each pod name, and Kubernetes limits the resulting pod hostname to 63 characters. The cap keeps room for ordinals 0-9. Set `fullnameOverride` to a shorter name if you run more than 10 runner replicas. A release name that is too long for the cap gets a short hash in the privileged name, so two long release names stay unique.

## Privileged Isolation

Use `runner.isolation: privileged` on clusters that cannot install sysbox. This includes immutable-rootfs distributions such as Talos, Bottlerocket, Flatcar, and Fedora CoreOS, where sysbox cannot modify the node filesystem or the containerd configuration.

The security boundary is weaker than sysbox. The runner container runs with `privileged: true` in the host user namespace. An escape from the runner container reaches the node. Because of this, the chart refuses to render until you acknowledge the trade-off:

```yaml
dataPlane:
  mode: in-cluster
runner:
  isolation: privileged
  acknowledgePrivileged: true
```

Requirements and recommendations:

- The namespace needs Pod Security Admission level `privileged`: `kubectl label namespace n8n-sandbox pod-security.kubernetes.io/enforce=privileged`. A PSA denial surfaces as an event on the StatefulSet, not as a failing pod.
- Prefer a dedicated node pool for the runner via `runner.privileged.scheduling`.
- Platforms that block privileged containers outright (for example GKE Autopilot) cannot use this isolation; use `dataPlane.mode: external` there.

When `runner.dockerDataRoot.persistence` is disabled, the chart mounts an `emptyDir` with `runner.dockerDataRoot.emptyDir.sizeLimit` at `/var/lib/docker`, so the inner Docker daemon's layers are bounded. With disk quotas the volume holds the quota pool image instead; see [Disk Quotas](#disk-quotas).

Two hardening options:

- **Kubernetes user namespaces.** On Kubernetes ≥ 1.33 with containerd ≥ 2.0, set `runner.privileged.runtime.hostUsers: false`. Container root then maps to an unprivileged host UID, and the privileged capabilities are scoped to the pod's user namespace. Verify that sandbox creation and resource limits work on your platform before you rely on it; inner-Docker cgroup management inside a user namespace depends on the node's kernel and runtime versions. Disk quotas (`defaultDiskQuotaMb`) do not work inside a user namespace.
- **VM-based RuntimeClass.** Set `runner.privileged.runtime.runtimeClassName` to a VM runtime such as Kata Containers. `privileged: true` then applies inside the guest VM, not on the node. Talos ships an official kata-containers system extension. This needs bare-metal nodes or nested virtualization.

Do not set `runner.privileged.runtime.runtimeClassName: sysbox-runc`; the chart fails the render and points you to `runner.isolation: sysbox`.

## Migrating from 0.3.x

Chart 0.4.0 restructured the data-plane values. The render fails with a message that names the new key if you still set an old key. Existing `sysbox` releases upgrade without workload changes: the runner resource names and labels are unchanged, so the StatefulSet is untouched.

| Old key | New key |
| --- | --- |
| `dataPlane.mode: sysbox` | `dataPlane.mode: in-cluster` (sysbox is the default isolation) |
| `sysboxRunner.enabled: false` | `dataPlane.mode: external` |
| `sysboxRunner.runtime.*` | `runner.sysbox.runtime.*` |
| `sysboxRunner.scheduling.*` | `runner.sysbox.scheduling.*` |
| `sysboxRunner.*` (all other keys) | `runner.*` (same key names) |
| `networkPolicy.sysboxRunner` | `networkPolicy.runner` |
| `monitoring.serviceMonitor.sysboxRunner` | `monitoring.serviceMonitor.runner` |

## Sysbox Scheduling Defaults

The default runner scheduling follows the Sysbox Kubernetes convention:

```yaml
runner:
  sysbox:
    runtime:
      runtimeClassName: sysbox-runc
      hostUsers: false
    scheduling:
      nodeSelector:
        sysbox-install: "yes"
      tolerations:
      - key: sysbox-runtime
        operator: Equal
        value: not-running
        effect: NoSchedule
```

The Sysbox installer labels the nodes `sysbox-install=yes` and taints them `sysbox-runtime=not-running:NoSchedule` until the runtime is ready. The default selector and toleration match that convention.

`hostUsers: false` asks Kubernetes to run the pod in a user namespace rather than the host user namespace. This is required by some Kubernetes/Sysbox setups for the runner pod to start. If your cluster does not support this field, set `runner.sysbox.runtime.hostUsers: null` to omit it.

If inner Docker cannot use `overlay2` in that environment, set `runner.config.dockerStorageDriver` to another dockerd storage driver such as `vfs`. This is slower than `overlay2`, but avoids nested overlayfs mounts.

For `overlay2`, prefer mounting a dedicated per-runner volume at the inner Docker data root so dockerd does not place its graph on the runner container filesystem:

```yaml
runner:
  config:
    dockerStorageDriver: overlay2
  dockerDataRoot:
    persistence:
      enabled: true
      size: 64Gi
      accessModes:
        - ReadWriteOnce
```

The chart renders this as a StatefulSet `volumeClaimTemplates` entry mounted at the inner Docker data root, so each runner replica gets its own Docker data root. Do not share one Docker data root volume across runner pods; the inner Docker daemon requires exclusive access to its graph.

If your cluster uses a dedicated node pool with custom labels and taints, override them through values:

```yaml
runner:
  sysbox:
    scheduling:
      nodeSelector:
        nodetype: sysbox
      tolerations:
        - key: dedicated
          operator: Equal
          value: sysbox
          effect: NoSchedule
```

## Disk Quotas

`runner.config.defaultDiskQuotaMb` above 0 caps the writable layer of each sandbox. To enforce the cap, the runner allocates a loopback xfs image, mounts it with `prjquota` at `/var/lib/docker`, and runs the inner Docker daemon against that mount.

The mount does not bound the image, so the image needs a bounded volume of its own. When the chart owns a Docker data root volume (`runner.dockerDataRoot.persistence.enabled`, or the `emptyDir` of privileged isolation), it mounts that volume at `/var/lib/docker-pool` instead and puts the image there. Without such a volume the image would land on the container filesystem and could fill the node disk, so the render fails.

The chart therefore requires an explicit pool size that fits the volume. With the default `sysbox` isolation, enable persistence so the volume exists:

```yaml
runner:
  config:
    defaultDiskQuotaMb: "2048"
    diskQuotaPoolSizeGb: "60"
  dockerDataRoot:
    persistence:
      enabled: true
      size: 64Gi
```

With `runner.isolation: privileged` and persistence disabled, the `emptyDir` holds the pool image instead. Set `runner.dockerDataRoot.emptyDir.sizeLimit` to at least `diskQuotaPoolSizeGb`:

```yaml
runner:
  isolation: privileged
  acknowledgePrivileged: true
  config:
    defaultDiskQuotaMb: "2048"
    diskQuotaPoolSizeGb: "60"
  dockerDataRoot:
    emptyDir:
      sizeLimit: 64Gi
```

`runner.config.diskQuotaPoolPath` overrides the image path. When it points at a volume you mount yourself (for example through `runner.extraVolumes`), the chart skips the size comparison. You then own the fit between the pool and that volume. Keep the override on a volume with a size limit; the mount that the image backs does not bound the image.

The render fails in three cases:

- No volume holds the pool image. This happens when `runner.dockerDataRoot.persistence` is disabled, isolation is `sysbox`, and `diskQuotaPoolPath` is empty. It is the default configuration, so enable persistence or set an explicit path.
- `runner.config.diskQuotaPoolSizeGb` is not a positive whole number of GB. The chart never falls back to the derived pool size, because that size scales with `runner.config.capacityTotal`. A 2048 MB per-sandbox quota and the default `capacityTotal` of 1000 derive a 2400 GB pool.
- `runner.config.diskQuotaPoolSizeGb` is larger than the chart-owned volume.

If the node kernel lacks xfs quota support, the pool mount fails. The runner then logs `disk quota enforcement: DISABLED`, the inner Docker daemon uses the container filesystem, and the volume stays unused. Watch that log line after you enable quotas.

## Existing Auth Secret

For production, prefer creating the auth Secret outside Helm and referencing it:

```yaml
auth:
  existingSecret: n8n-sandbox-auth
  secretKeys:
    apiKeys: api-keys
    runnerRegistrationToken: runner-registration-token
    runnerApiKey: runner-api-key
    runnerApiKeys: runner-api-keys
```

If `auth.existingSecret` is empty, the chart creates an opaque Secret from `auth.generated`:

```yaml
auth:
  existingSecret: ""
  generated:
    apiKeys: replace-with-random-api-key
    runnerRegistrationToken: replace-with-random-registration-token
    runnerApiKey: replace-with-random-runner-api-key
    runnerApiKeys: replace-with-random-runner-api-key
```

The chart fails rendering when any generated auth value is empty or `changeme`. Do not expose the API with placeholder credentials.

The API uses `apiKeys`, `runnerRegistrationToken`, and `runnerApiKey`. The runner uses `runnerApiKeys` and `runnerRegistrationToken`.

## TLS Secrets

The service requires gRPC mTLS between the API and runners. Configure it in one place with `tls.mode`:

- `tls.mode=existingSecret`: create the TLS Secrets yourself and point the chart at them.
- `tls.mode=certManager`: the chart renders cert-manager `Certificate` resources for the required Secrets.

The chart mounts the Secrets and wires the corresponding `SANDBOX_*_TLS_*` environment variables.

Expected default key names are `tls.crt`, `tls.key`, and `ca.crt`; override the `*FileKey` values if your Secret uses different names.

```yaml
tls:
  mode: existingSecret
  certificates:
    apiRegistrationServer:
      secretName: n8n-sandbox-api-registration-tls
    apiControlClient:
      secretName: n8n-sandbox-api-control-client-tls
    runnerRegistrationClient:
      secretName: n8n-sandbox-runner-registration-tls
    runnerControlServer:
      secretName: n8n-sandbox-runner-control-tls
```

## cert-manager

If cert-manager is installed, enable certificate generation and reference an existing private `Issuer` or `ClusterIssuer`:

```yaml
tls:
  mode: certManager
  certManager:
    issuerRef:
      name: sandbox-ca
      kind: ClusterIssuer
      group: cert-manager.io
```

This renders four `Certificate` resources:

- API registration server certificate with `server auth`.
- API control client certificate with `client auth`.
- Runner registration client certificate with `client auth`.
- Runner control server certificate with `server auth`.

The generated Secret names match `tls.certificates.*.secretName`, so the workloads mount the same Secrets whether cert-manager or an external process creates them.

For the API registration server certificate, the chart includes the API Service DNS names. For the runner control server certificate, the chart includes the headless Service DNS names and a wildcard pod DNS name for StatefulSet runners. Add extra SANs through:

```yaml
tls:
  certificates:
    apiRegistrationServer:
      dnsNames:
        - sandbox-api.example.internal
    runnerControlServer:
      dnsNames:
        - "*.custom-runner.sandbox.svc.cluster.local"
```

## Traefik Ingress

API ingress is optional. Enable it when the API should be addressable through Traefik or another Ingress controller:

```yaml
api:
  ingress:
    enabled: true
    className: traefik
    hosts:
      - host: sandbox.example.com
        paths:
          - path: /
            pathType: Prefix
    tls:
      - secretName: sandbox-api-public-tls
        hosts:
          - sandbox.example.com
```

If Traefik is configured through annotations instead of `ingressClassName`, leave `className` empty and set `api.ingress.annotations`.

The Ingress exposes only the public HTTP API port. The private runner registration gRPC port stays on the ClusterIP Service.

## NetworkPolicy

`networkPolicy.enabled` is disabled by default because many clusters use provider-specific policy CRDs or manage network policy outside application charts.

When enabled, the chart renders Kubernetes `NetworkPolicy` resources for the API and the in-cluster runner:

- API HTTP remains reachable from all sources by default so an existing ingress controller continues to work. Set `networkPolicy.api.httpIngressFrom` to restrict it to your ingress controller.
- API registration gRPC is reachable from the in-chart runner by default. In external data-plane mode it is denied unless peers are added through `networkPolicy.api.grpcIngressFrom`.
- Runner HTTP/control ports are reachable from the in-chart API by default. Add peers through `networkPolicy.runner.ingressFrom` only if another component needs direct runner access.

Example restricting public API traffic to an ingress controller namespace:

```yaml
networkPolicy:
  enabled: true
  api:
    httpIngressFrom:
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: ingress-nginx
```

## Prometheus Metrics

The API and the in-cluster runner expose Prometheus metrics on their HTTP port at `/metrics`. If your cluster uses Prometheus Operator, enable `ServiceMonitor` resources:

```yaml
monitoring:
  serviceMonitor:
    enabled: true
    labels:
      release: kube-prometheus-stack
```

This renders one `ServiceMonitor` for the API Service and, when the in-chart runner is enabled, one for the runner headless Service. It also enables the matching `/metrics` handlers in the API and runner containers. Use `monitoring.serviceMonitor.api.enabled` or `monitoring.serviceMonitor.runner.enabled` to disable either scrape target.

## Runner Identity

The in-cluster runner is deployed as a StatefulSet with a headless Service so each runner pod has direct, DNS-based addressability from the API:

```text
http://<pod>.<runner-service>.<namespace>.svc.cluster.local:8080
<pod>.<runner-service>.<namespace>.svc.cluster.local:9091
```

The API must call the specific runner that owns a sandbox; a load-balanced Service is not enough for exec/files/control operations. Stable pod DNS also makes the runner control gRPC certificate SANs practical.

If a runner dies, sandboxes on that runner should be treated as lost.

## API Persistence

**SQLite (default):** Keep `api.replicaCount: 1`. `api.persistence.enabled` is enabled by default so sandbox routing state survives API pod restarts.

**Postgres (multi-pod):** Set `api.replicaCount` to 2 or more and configure Postgres via environment variables:

```yaml
api:
  replicaCount: 3
  persistence:
    enabled: false
  config:
    store: postgres
  extraEnv:
    - name: SANDBOX_API_POSTGRES_HOST
      value: your-postgres-host
    - name: SANDBOX_API_POSTGRES_USER
      value: sandbox
    - name: SANDBOX_API_POSTGRES_DB
      value: sandbox
    - name: SANDBOX_API_POSTGRES_PASSWORD
      valueFrom:
        secretKeyRef:
          name: sandbox-api-postgres
          key: password
```

Postgres integration tests (optional): run `go test -tags=integration ./internal/api/...` with `SANDBOX_TEST_POSTGRES_HOST` and related env vars set.
