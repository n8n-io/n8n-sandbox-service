# Quickstart: kubernetes

This guide covers running the n8n Sandbox Service on kubernetes.

## Contents

- [Install sysbox](#install-sysbox)
- [Use the sandbox service chart](#use-the-sandbox-service-chart)
- [Verify](#verify)

## Install sysbox

* Create a new nodepool and label it, so you can identify the nodes that are supposed to run the sysbox sandbox workloads.
* Build a helm chart based on sysbox' [own manifests](https://raw.githubusercontent.com/nestybox/sysbox/master/sysbox-k8s-manifests/sysbox-install.yaml). Update the node selectors to match the labels you gave to your nodepool.

## Use the sandbox service chart

Write a wrapper helm chart that includes the n8n-sandbox-service helm chart as a dependency. Then add additional resources you might need, depending on your setup, like: an external secret for the Docker credentials/API keys, an ingressroute or a CA certificate. For more configuration options, refer to the chart's [README](file://../charts/n8n-sandbox-service/README.md).

## Verify

Check the API health endpoint:

```bash
kubectl port-forward deploy/sandbox-n8n-sandbox-service-api 8080:8080
curl http://localhost:8080/healthz
```

If you used an ingress you can also use that to check the API health.
