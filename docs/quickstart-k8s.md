# Quickstart: Kubernetes

This guide covers running the n8n Sandbox Service with an in-cluster Sysbox runner.

Sysbox must install a runtime binary on the node and change the node's containerd configuration. Immutable-rootfs distributions such as Talos, Bottlerocket, Flatcar, and Fedora CoreOS do not permit that. On those clusters, skip to [Immutable-rootfs distributions](#immutable-rootfs-distributions-privileged-isolation).

## 1. Create a Sysbox node pool

Use a dedicated Linux node pool for Sysbox workloads. Sysbox changes the node runtime setup, so keep these nodes separate from regular application nodes.

Follow the upstream [Sysbox Kubernetes requirements](https://github.com/nestybox/sysbox/blob/master/docs/user-guide/install-k8s.md):

- Kubernetes `1.32` to `1.35`.
- Ubuntu Noble, Jammy, Focal, or Bionic workers.
- At least 4 CPUs and 4 GB RAM per node.
- containerd `2.0.5` or newer for native Kubernetes user namespaces. If containerd is not suitable, the Sysbox installer may configure CRI-O and restart kubelet on the node.

Provider notes:

- GKE: use Standard clusters with `UBUNTU_CONTAINERD` node pools. Autopilot is not suitable because it [blocks privileged containers and most hostPath mounts](https://cloud.google.com/kubernetes-engine/docs/concepts/autopilot-security), both of which the [Sysbox installer](https://raw.githubusercontent.com/nestybox/sysbox/master/sysbox-k8s-manifests/sysbox-install.yaml) needs.
- EKS: use Ubuntu workers, usually through `eksctl` or a [custom AMI launch template](https://docs.aws.amazon.com/eks/latest/userguide/launch-templates.html). New managed node groups default to [Amazon Linux 2023](https://docs.aws.amazon.com/eks/latest/userguide/eks-optimized-ami.html), while Sysbox documents EKS with Ubuntu.
- AKS: the default Ubuntu/containerd workers are supported by Sysbox, but the default 2 vCPU nodes are too small. Use at least 4 vCPUs per node.

Label the nodes that should receive Sysbox:

```bash
kubectl label nodes <node-name> sysbox-install=yes
```

## 2. Install Sysbox

Apply the upstream installer, or wrap the same manifest in your own Helm chart if you manage cluster add-ons that way:

```bash
kubectl apply -f https://raw.githubusercontent.com/nestybox/sysbox/master/sysbox-k8s-manifests/sysbox-install.yaml
```

Wait for the installer to finish:

```bash
kubectl -n kube-system logs -f ds/sysbox-deploy-k8s
kubectl get runtimeclass sysbox-runc
kubectl get nodes -l sysbox-runtime=running
```

The Sysbox troubleshooting guide has the expected install log flow and common kubelet/runtime checks: [troubleshoot-k8s.md](https://github.com/nestybox/sysbox/blob/master/docs/user-guide/troubleshoot-k8s.md).

## 3. Deploy the sandbox service

Create a wrapper Helm chart that depends on `n8n-sandbox-service`, then add your own Secrets, TLS issuer, ingress, and registry credentials. See the chart [README](../charts/n8n-sandbox-service/README.md) for all values.

For a containerd setup with [Kubernetes user namespaces](https://kubernetes.io/docs/concepts/workloads/pods/user-namespaces/), keep the runner on Sysbox and use `hostUsers: false`:

```yaml
dataPlane:
  mode: in-cluster

runner:
  isolation: sysbox
  sysbox:
    runtime:
      runtimeClassName: sysbox-runc
      hostUsers: false
```

If your Sysbox nodes use CRI-O for user namespaces, omit `hostUsers` and add the CRI-O annotation documented by Sysbox:

```yaml
runner:
  sysbox:
    runtime:
      runtimeClassName: sysbox-runc
      hostUsers: null
  podAnnotations:
    io.kubernetes.cri-o.userns-mode: "auto:size=65536"
```

If your node pool uses custom labels or taints, override the runner scheduling values:

```yaml
runner:
  sysbox:
    scheduling:
      nodeSelector:
        sysbox-install: null
        nodetype: sysbox
      tolerations:
        - key: dedicated
          operator: Equal
          value: sysbox
          effect: NoSchedule
```

Setting a default selector key to `null` removes it from the rendered pod selector.

## 4. Verify

Check that the API is healthy:

```bash
kubectl port-forward deploy/sandbox-n8n-sandbox-service-api 8080:8080
curl http://localhost:8080/healthz
```

If the API is exposed through an ingress, use the ingress URL instead.

For Prometheus Operator, enable the chart's optional `ServiceMonitor` resources:

```yaml
monitoring:
  serviceMonitor:
    enabled: true
```

## Immutable-rootfs distributions (privileged isolation)

Sysbox cannot install on distributions with a read-only root filesystem and machine-managed containerd configuration. Verified on Talos; the same constraint applies to Bottlerocket, Flatcar, and Fedora CoreOS. Use `runner.isolation: privileged` there. It runs the same Docker-in-Docker runner with `privileged: true` instead of the sysbox runtime, so any node can run it.

The security boundary is weaker than sysbox: an escape from the runner container reaches the node. The chart refuses to render until you acknowledge this:

```yaml
dataPlane:
  mode: in-cluster

runner:
  isolation: privileged
  acknowledgePrivileged: true
```

The namespace needs Pod Security Admission level `privileged`:

```bash
kubectl label namespace <namespace> pod-security.kubernetes.io/enforce=privileged
```

Prefer a dedicated node pool via `runner.privileged.scheduling`, and keep NetworkPolicy enabled. Platforms that block privileged containers (for example GKE Autopilot) cannot use this isolation.

Two hardening options, both described in the chart [README](../charts/n8n-sandbox-service/README.md#privileged-isolation):

- `runner.privileged.runtime.hostUsers: false` on Kubernetes ≥ 1.33 with containerd ≥ 2.0. This scopes the privileged capabilities to a pod user namespace. Verify sandbox creation and resource limits on your platform first.
- `runner.privileged.runtime.runtimeClassName` set to a VM runtime such as Kata Containers, so `privileged: true` applies inside a guest VM. Talos ships an official kata-containers system extension.

## Troubleshooting

**The runner logs `chmod /var/lib/docker: operation not permitted`.** The inner Docker daemon cannot chmod its data root, so it exits and the runner never becomes ready. A `PersistentVolume` is mounted at `/var/lib/docker` and the pod runs in a user namespace, so the volume root belongs to a UID outside the pod's mapping. Chart 0.6.3 and later fail the render on this combination. Disable `runner.dockerDataRoot.persistence` to use the bounded `emptyDir` instead, and see the chart README section [Docker Data Root](../charts/n8n-sandbox-service/README.md#docker-data-root).

**Install succeeds but no runner pod appears.** Admission denied the pod, so `kubectl get pods` shows nothing and the error lands on the StatefulSet. Inspect the StatefulSet events:

```bash
kubectl -n <namespace> describe statefulset
```

Two causes end up here: `RuntimeClass "sysbox-runc" not found` (sysbox is not installed on the cluster — see the note about immutable-rootfs distributions above), or Pod Security Admission rejects the privileged runner.

**The runner pod stays in `Pending`.** The scheduler found no node for it. Read the pod events:

```bash
kubectl -n <namespace> describe pod -l app.kubernetes.io/component=sysbox-runner
```

Use `app.kubernetes.io/component=privileged-runner` for privileged isolation. A `0/N nodes are available` message means no node matches the `sysbox-install: "yes"` node selector, or the nodes carry a taint the pod does not tolerate. Label the sysbox nodes, or override `runner.sysbox.scheduling`.

For `mount through procfd: operation not permitted`, first check the user-namespace configuration:

- containerd: use a supported Kubernetes/containerd combination and `hostUsers: false`. Relevant Sysbox issues: [#958](https://github.com/nestybox/sysbox/issues/958), [#1006](https://github.com/nestybox/sysbox/issues/1006).
- CRI-O: use `io.kubernetes.cri-o.userns-mode: "auto:size=65536"` and `hostUsers: null`.

In our EKS testing, the same Sysbox and containerd setup failed on `1.32.13-eks-0247562` and worked on `1.35.4-eks-40737a8`. Treat this as a platform-version signal, not an upstream guarantee.

For `failed to find runtime handler sysbox-runc`, confirm the `RuntimeClass` exists and check the node runtime config. See Sysbox's [Kubernetes troubleshooting guide](https://github.com/nestybox/sysbox/blob/master/docs/user-guide/troubleshoot-k8s.md#pod-stuck-in-creating-status).
