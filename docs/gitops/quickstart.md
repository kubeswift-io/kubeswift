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
  url: oci://ghcr.io/kubeswift-io/charts/kubeswift
  ref: { semver: "0.13.x" }
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

Three load-bearing choices:

- **`crds: CreateReplace` on BOTH install and upgrade.** Helm's CLI cannot
  upgrade CRDs at all; Flux's helm-controller can, and this is what tells it to.
  KubeSwift CRDs evolve with every release, and a stale CRD makes the apiserver
  silently strip new fields (see [overview.md](overview.md)).
- **Pin the chart to a minor range, not `>=0.1.0`.** KubeSwift is pre-1.0 and
  its APIs are `v1alpha1`: a wide-open range lets Flux roll the platform across
  a breaking minor unattended. `"0.13.x"` takes patches automatically and makes
  a minor bump a reviewed commit. Pin exactly (`0.13.13`) if you want even
  patches gated.
- **`webhook.enabled: true` needs cert-manager** on the cluster (the chart's
  Certificate/Issuer objects require its CRDs). Without cert-manager, set it
  false — admission validation is then skipped and the controllers' reconcile-
  time checks are your only guard. Note that
  `swiftGuest.allowedHostPathPrefixes` is webhook-enforced, so it does nothing
  with the webhook off.

### Image tags come from the chart version

Every KubeSwift image tag defaults to the chart's `appVersion`, so pinning the
chart pins the whole platform. You do not need to list image tags in `values`,
and you should not: hand-pinned tags are how a controller ends up newer than
the RBAC its chart shipped.

The one exception is the optional web console: **`ui.image.tag` is not
chart-derived**, because `kubeswift-ui` releases from its own repository on its
own cadence. A chart bump will not move it. If you enable the UI, raise that tag
deliberately when you want a newer console.

### Values worth setting explicitly

| Value | Default | Why it matters under GitOps |
|---|---|---|
| `launcherSAGate.enabled` | `true` | A ValidatingAdmissionPolicy closing a privilege escalation on launcher ServiceAccounts. Rendered only where the cluster serves `ValidatingAdmissionPolicy` (GA in k8s 1.30+); on older clusters it is **silently skipped** and the namespace becomes your trust boundary. Leave on. |
| `scopedLauncherRBAC.enabled` | `false` | Retires the shared namespace-wide launcher binding, leaving per-pod grants. Enabling it **deletes a live grant** — a one-way change to make deliberately, not as part of an unrelated values edit. |
| `swiftGuest.allowedHostPathPrefixes` | `[]` (deny all) | Host paths a SwiftGuest may mount into its privileged launcher. Empty denies everything. Anything you add here is effectively node-root for whoever can author a SwiftGuest. |
| `monitoring.*` | off | Needs the Prometheus Operator CRDs (`monitoring.coreos.com`) already present. Enabling it before they exist fails the HelmRelease. |
| `gateway.*`, `ui.*`, `federation.*` | off | Multi-cluster console and fleet federation. `federation.selfRegister` makes the chart create a fleet `Cluster` for this cluster — do not also declare that object in Git. |

## 3. Layers 2 and 3

`kubeswift-infra.yaml` and `kubeswift-workloads.yaml` are Flux Kustomizations
pointing at `infrastructure/kubeswift/` and `workloads/<env>/`, chained with
`dependsOn`. See [infrastructure.md](infrastructure.md) and
[workloads.md](workloads.md).

## 4. Verify

```bash
flux get kustomizations            # all Ready
kubectl get swiftimages            # imports running/Ready
kubectl get swiftkernels           # per-node kernel artifacts, if used
kubectl get swiftguests,swiftguestpools -A
```

Confirm the CRDs actually moved with the chart, which is the failure this
layout exists to prevent:

```bash
kubectl get crd swiftguests.swift.kubeswift.io \
  -o jsonpath='{.metadata.annotations.controller-gen\.kubebuilder\.io/version}{"\n"}'
kubectl explain swiftimage.spec.importStorageClassName   # a v0.13.11 field
```

If `kubectl explain` cannot find a field your chart version should have, the
CRDs are stale and every manifest using that field is being silently stripped.
