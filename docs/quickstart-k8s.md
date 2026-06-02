# Quickstart: Kubernetes

This guide covers running the n8n Sandbox Service with an in-cluster Sysbox runner.

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
  mode: sysbox

sysboxRunner:
  runtime:
    runtimeClassName: sysbox-runc
    hostUsers: false
```

If your Sysbox nodes use CRI-O for user namespaces, omit `hostUsers` and add the CRI-O annotation documented by Sysbox:

```yaml
sysboxRunner:
  runtime:
    runtimeClassName: sysbox-runc
    hostUsers: null
  podAnnotations:
    io.kubernetes.cri-o.userns-mode: "auto:size=65536"
```

If your node pool uses custom labels or taints, override the runner scheduling values:

```yaml
sysboxRunner:
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

## Troubleshooting

For `mount through procfd: operation not permitted`, first check the user-namespace configuration:

- containerd: use a supported Kubernetes/containerd combination and `hostUsers: false`. Relevant Sysbox issues: [#958](https://github.com/nestybox/sysbox/issues/958), [#1006](https://github.com/nestybox/sysbox/issues/1006).
- CRI-O: use `io.kubernetes.cri-o.userns-mode: "auto:size=65536"` and `hostUsers: null`.

In our EKS testing, the same Sysbox and containerd setup failed on `1.32.13-eks-0247562` and worked on `1.35.4-eks-40737a8`. Treat this as a platform-version signal, not an upstream guarantee.

For `failed to find runtime handler sysbox-runc`, confirm the `RuntimeClass` exists and check the node runtime config. See Sysbox's [Kubernetes troubleshooting guide](https://github.com/nestybox/sysbox/blob/master/docs/user-guide/troubleshoot-k8s.md#pod-stuck-in-creating-status).
