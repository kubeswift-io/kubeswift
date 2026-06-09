# KubeSwift GitOps reference (FluxCD)

A fork-and-adapt starting point for managing KubeSwift declaratively with
[FluxCD](https://fluxcd.io/), implementing the three-layer model from
[`docs/design/gitops-flux.md`](../../docs/design/gitops-flux.md). Operator
documentation lives in [`docs/gitops/`](../../docs/gitops/).

```
clusters/<env>/          # per-cluster Flux entry points
  kubeswift-platform.yaml   # Layer 1: OCIRepository + HelmRelease (chart + CRDs)
  kubeswift-infra.yaml      # Layer 2: Kustomization -> infrastructure/kubeswift
  kubeswift-workloads.yaml  # Layer 3: Kustomization -> workloads/<env>
infrastructure/kubeswift/   # shared classes, images, seeds, GPU profiles
workloads/<env>/            # environment-specific guests + pools
```

Apply order is enforced with `dependsOn`: **platform (CRDs) → infra →
workloads**. Image imports are async — `wait: false` on the infra
Kustomization lets workloads apply while imports run; SwiftGuests wait in
`Pending` until their SwiftImage is `Ready`.

## Using it

1. Copy this tree into your own Git repository.
2. Replace the SSH key in `infrastructure/kubeswift/seeds/default.yaml`
   (and move any real credentials to a SOPS-encrypted Secret —
   see [`docs/gitops/secrets.md`](../../docs/gitops/secrets.md)).
3. Bootstrap Flux against the repo:
   `flux bootstrap github --owner=<you> --repository=<repo> --path=clusters/production`
4. Watch the layers come up: `flux get kustomizations` /
   `kubectl get swiftimages,swiftguests -A`.

## Validating locally

Every directory is a valid Kustomize entry point:

```bash
kubectl kustomize clusters/production
kubectl kustomize infrastructure/kubeswift | kubectl apply --dry-run=server -f -
kubectl kustomize workloads/production    | kubectl apply --dry-run=server -f -
```

(The server-side dry-run requires the KubeSwift CRDs on the cluster; the Flux
CRs in `clusters/` additionally require Flux's CRDs.)
