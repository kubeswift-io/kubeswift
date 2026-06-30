# Remote Cluster Install

Use this path for **remote clusters** (cloud, on-prem, etc.): install from the OCI Helm chart. No local build required. For kind/minikube, use [local cluster](local-cluster.md) instead.

## When to use remote vs local

| Scenario | Use |
|----------|-----|
| Remote cluster, production-like | [Remote cluster](remote-cluster.md) — Helm OCI |
| kind, minikube, development | [Local cluster](local-cluster.md) — build, load, deploy |

## Prerequisites

- Helm 3+
- `kubectl` configured for the cluster
- Worker nodes with KVM; run [preflight](../operator/worker-node-preflight.md) before running guests

## Install

```bash
helm install kubeswift oci://ghcr.io/kubeswift-io/charts/kubeswift \
  --version 0.1.0 \
  -n kubeswift-system \
  --create-namespace
```

Version guide: dev `0.0.0-dev.<shortsha>`, RC `X.Y.Z-rc.N`, stable `X.Y.Z`. See [Helm OCI](helm-oci.md).

## Post-install

Apply RBAC in each namespace where SwiftGuests run:

```bash
kubectl apply -k config/rbac -n default
```

Then create SwiftGuests. Run [smoke test](../operator/smoke-verification.md#quick-walkthrough) to validate.

[Helm OCI](helm-oci.md) · [Local cluster](local-cluster.md)
