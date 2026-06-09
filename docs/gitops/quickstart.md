# GitOps quickstart — Flux + the platform layer

## 1. Install Flux and bootstrap

Follow the [Flux bootstrap docs](https://fluxcd.io/flux/installation/) for your
Git host. With the [reference layout](../../examples/gitops-flux/) copied into
your repo:

```bash
flux bootstrap github \
  --owner=<org> --repository=<fleet-repo> \
  --branch=main --path=clusters/production
```

Flux installs itself, then reconciles everything under
`clusters/production/`.

## 2. Layer 1 — the KubeSwift platform

`clusters/<env>/kubeswift-platform.yaml` declares the chart source and release
([full file](../../examples/gitops-flux/clusters/production/kubeswift-platform.yaml)):

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata: { name: kubeswift, namespace: flux-system }
spec:
  interval: 10m
  url: oci://ghcr.io/projectbeskar/charts/kubeswift
  ref: { semver: ">=0.1.0" }
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata: { name: kubeswift, namespace: flux-system }
spec:
  interval: 10m
  targetNamespace: kubeswift-system
  install: { createNamespace: true, crds: CreateReplace }
  upgrade: { crds: CreateReplace }
  chartRef: { kind: OCIRepository, name: kubeswift, namespace: flux-system }
  values:
    webhook: { enabled: true }
```

Two load-bearing choices:

- **`crds: CreateReplace` on BOTH install and upgrade.** Helm's default skips
  CRD upgrades; KubeSwift CRDs evolve with every release, and a stale CRD makes
  the apiserver silently strip new fields (see [overview.md](overview.md)).
- **`webhook.enabled: true` needs cert-manager** on the cluster (the chart's
  Certificate/Issuer objects require its CRDs). Without cert-manager, set it
  false — admission validation is then skipped and the controllers' reconcile-
  time checks are your only guard.

## 3. Layers 2 and 3

`kubeswift-infra.yaml` and `kubeswift-workloads.yaml` are Flux Kustomizations
pointing at `infrastructure/kubeswift/` and `workloads/<env>/`, chained with
`dependsOn`. See [infrastructure.md](infrastructure.md) and
[workloads.md](workloads.md).

## 4. Verify

```bash
flux get kustomizations            # all Ready
kubectl get swiftimages            # imports running/Ready
kubectl get swiftguests,swiftguestpools -A
```
