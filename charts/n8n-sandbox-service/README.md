# n8n Sandbox Service Helm Chart

This chart deploys the n8n Sandbox Service API and, optionally, the in-cluster sysbox/Docker-in-Docker runner.

## Install

Create or provide:

- An auth Secret with API keys and runner registration tokens, or set `auth.*` values and let the chart create one.
- TLS Secrets for the API registration server, API control client, runner registration client, and runner control server, or set `tls.mode=certManager`.
- A Sysbox-ready node pool if `sysboxRunner.enabled=true`.

```sh
helm upgrade --install n8n-sandbox-service ./charts/n8n-sandbox-service \
  --namespace n8n-sandbox \
  --create-namespace
```

## Data Plane Mode

Use `dataPlane.mode: sysbox` for the in-cluster sysbox/DinD runner. Use `dataPlane.mode: external` when runners live outside Kubernetes. In external mode, the chart renders the API resources but does not render the sysbox runner StatefulSet.

The runner binary selects its sandbox backend with `SANDBOX_RUNNER_BACKEND`.
This chart only renders the sysbox runner today, so `sysboxRunner.config.backend`
defaults to `docker`. The `firecracker` backend is wired in the binary for
external/host deployments but is not implemented or charted yet.

## Sysbox Scheduling Defaults

The default runner scheduling follows the Sysbox Kubernetes convention:

```yaml
sysboxRunner:
  runtime:
    runtimeClassName: sysbox-runc
    hostUsers: false
  scheduling:
    nodeSelector:
      sysbox-runtime: running
    tolerations: []
```

`hostUsers: false` asks Kubernetes to run the pod in a user namespace rather than the host user namespace. This is required by some Kubernetes/Sysbox setups for the runner pod to start. If your cluster does not support this field, set `sysboxRunner.runtime.hostUsers: null` to omit it.

If inner Docker cannot use `overlay2` in that environment, set `sysboxRunner.config.dockerStorageDriver` to another dockerd storage driver such as `vfs`. This is slower than `overlay2`, but avoids nested overlayfs mounts.

For `overlay2`, prefer mounting a dedicated per-runner volume at the inner Docker data root so dockerd does not place its graph on the runner container filesystem:

```yaml
sysboxRunner:
  config:
    dockerStorageDriver: overlay2
  dockerDataRoot:
    persistence:
      enabled: true
      size: 64Gi
      accessModes:
        - ReadWriteOnce
```

The chart renders this as a StatefulSet `volumeClaimTemplates` entry mounted at `/var/lib/docker`, so each runner replica gets its own Docker data root. Do not share one `/var/lib/docker` volume across runner pods; the inner Docker daemon requires exclusive access to its graph.

If your cluster uses a dedicated node pool with custom labels and taints, override them through values:

```yaml
sysboxRunner:
  scheduling:
    nodeSelector:
      nodetype: sysbox
    tolerations:
      - key: dedicated
        operator: Equal
        value: sysbox
        effect: NoSchedule
```

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

When enabled, the chart renders Kubernetes `NetworkPolicy` resources for the API and in-cluster sysbox runner:

- API HTTP remains reachable from all sources by default so an existing ingress controller continues to work. Set `networkPolicy.api.httpIngressFrom` to restrict it to your ingress controller.
- API registration gRPC is reachable from the in-chart sysbox runner by default. In external data-plane mode it is denied unless peers are added through `networkPolicy.api.grpcIngressFrom`.
- Runner HTTP/control ports are reachable from the in-chart API by default. Add peers through `networkPolicy.sysboxRunner.ingressFrom` only if another component needs direct runner access.

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

## Runner Identity

The sysbox runner is deployed as a StatefulSet with a headless Service so each runner pod has direct, DNS-based addressability from the API:

```text
http://<pod>.<runner-service>.<namespace>.svc.cluster.local:8080
<pod>.<runner-service>.<namespace>.svc.cluster.local:9091
```

The API must call the specific runner that owns a sandbox; a load-balanced Service is not enough for exec/files/control operations. Stable pod DNS also makes the runner control gRPC certificate SANs practical.

If a runner dies, sandboxes on that runner should be treated as lost.

## API Persistence

Keep `api.replicaCount: 1` while the API uses its local SQLite store. `api.persistence.enabled` is enabled by default so sandbox routing state survives API pod restarts.
