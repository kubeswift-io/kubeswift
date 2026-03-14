# Helm OCI Install

Install KubeSwift from the OCI Helm chart. Use for **remote clusters**; for kind/minikube, use [local cluster](local-cluster.md).

## Image tags

The chart defaults image tags to match published images:
- **Stable/RC** (e.g. chart 0.1.0): `v0.1.0`
- **Dev** (e.g. chart 0.0.0-dev.abc1234): `sha-abc1234`

CI does not publish `latest`. For **local builds** (kind/minikube), use `make build-images`, `make load-images`, then `make deploy` instead of Helm—or override: `--set controllerManager.image.tag=latest --set swiftletd.image.tag=latest` (only if you built and loaded images locally).

## Chart (install / pull reference)

```
oci://ghcr.io/projectbeskar/charts/kubeswift
```

### Push vs install reference

Helm OCI uses different references for **push** vs **install/pull**:

| Use | Reference | Notes |
|-----|-----------|-------|
| **Push** | `oci://ghcr.io/projectbeskar/charts` | Parent repo only; Helm appends chart name from `Chart.yaml` and version from the package. Per [Helm docs](https://helm.sh/docs/registries/), the push reference must NOT contain the chart basename or tag. |
| **Install / pull** | `oci://ghcr.io/projectbeskar/charts/kubeswift` | Full path including chart name; used with `--version` for install. |

Stored artifact: `ghcr.io/projectbeskar/charts/kubeswift:<version>`.

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

## Migration: previously published charts

If charts were pushed before this fix using the full path (`oci://ghcr.io/projectbeskar/charts/kubeswift`) as the push destination, they may have landed at a nested path such as `ghcr.io/projectbeskar/charts/kubeswift/kubeswift:<version>`. Those charts would require a different install reference:

```bash
# Nested path (legacy, if applicable)
helm install kubeswift oci://ghcr.io/projectbeskar/charts/kubeswift/kubeswift --version <version> ...
```

Charts pushed after this fix use the correct path `ghcr.io/projectbeskar/charts/kubeswift:<version>`. Verify with `helm show chart oci://ghcr.io/projectbeskar/charts/kubeswift --version <version>` before installing.

[Releases](../releases.md) · [Remote cluster](remote-cluster.md)
