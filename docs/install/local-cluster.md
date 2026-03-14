# Local Cluster Install

Use this path for **kind** or **minikube**: build images locally, load them into the cluster (no registry), then deploy. For remote clusters, use [Helm OCI](helm-oci.md) instead.

## When to use local vs remote

| Scenario | Use |
|----------|-----|
| Development, CI, quick try | [Local cluster](local-cluster.md) — build, load, deploy |
| Remote cluster, production-like | [Remote cluster](remote-cluster.md) — Helm OCI |

## Prerequisites

- kind or minikube (Kubernetes 1.28+)
- `kubectl` configured
- Docker (or compatible)
- Worker nodes with KVM; run [preflight](../operator/worker-node-preflight.md) before smoke test

## Steps

```bash
make build-images
make load-images   # detects kind/minikube, loads controller-manager + swiftletd
make deploy
```

`deploy` applies CRDs and KubeSwift (namespace, controller-manager, swiftletd DaemonSet).

## Post-deploy

Apply RBAC in each namespace where SwiftGuests run:

```bash
kubectl apply -k config/rbac -n default
```

Then create SwiftGuests. Run `make smoke-test` to validate.

## Override image tags

```bash
make build-images IMAGE_TAG=v0.1.0
make load-images
make deploy
```

Or use kustomize to patch image refs in `config/default`.

## Undeploy

```bash
make undeploy
```

[Remote cluster](remote-cluster.md) · [Helm OCI](helm-oci.md) · [Smoke verification](../operator/smoke-verification.md)
