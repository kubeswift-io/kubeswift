# Helm OCI Install

Install KubeSwift from the OCI Helm chart. Use for **remote clusters**; for kind/minikube, use [local cluster](local-cluster.md).

## Chart

```
oci://ghcr.io/projectbeskar/charts/kubeswift
```

## Install

```bash
helm install kubeswift oci://ghcr.io/projectbeskar/charts/kubeswift \
  --version 0.1.0 \
  -n kubeswift-system \
  --create-namespace
```

## Version selection (dev / RC / stable)

| Release type | When | Chart version | Example |
|--------------|------|---------------|---------|
| **Dev** | Push to `main` | `0.0.0-dev.<shortsha>` | `0.0.0-dev.a1b2c3d` |
| **RC** | Tag `v*.*.*-rc.*` | `X.Y.Z-rc.N` | `0.1.0-rc.1` |
| **Stable** | Tag `v*.*.*` (no `-rc`) | `X.Y.Z` | `0.1.0` |

Use dev for bleeding edge; RC for pre-release; stable for production.

## Webhooks (optional)

Requires [cert-manager](https://cert-manager.io/):

```bash
helm install kubeswift oci://ghcr.io/projectbeskar/charts/kubeswift \
  --version 0.1.0 \
  -n kubeswift-system \
  --create-namespace \
  --set webhook.enabled=true
```

## Image overrides (air-gapped / custom registry)

```bash
helm install kubeswift oci://ghcr.io/projectbeskar/charts/kubeswift \
  --version 0.1.0 \
  -n kubeswift-system \
  --create-namespace \
  --set controllerManager.image.registry=my-registry.io \
  --set controllerManager.image.tag=v0.1.0 \
  --set swiftletd.image.registry=my-registry.io \
  --set swiftletd.image.tag=v0.1.0
```

[Releases](../releases.md) · [Remote cluster](remote-cluster.md)
