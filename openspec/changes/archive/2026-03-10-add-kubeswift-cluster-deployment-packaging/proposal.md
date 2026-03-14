## Why

The repository contains controllers, CRDs, RBAC, samples, and a smoke test, but no reproducible way to install KubeSwift into a cluster. The smoke test assumes CRDs and controllers are already deployed. Deployment packaging is the missing prerequisite: without it, the repo is not installable. Adding image build definitions, Kubernetes manifests, kustomizations, and Makefile targets makes KubeSwift installable. Smoke testing remains out of scope except as the consumer of the deployment path.

## What Changes

- Add container build definition for controller-manager (webhooks run in-process; no separate webhook server)
- Keep existing swiftletd image build; integrate into complete image build workflow
- Add Kubernetes manifests: namespace, service accounts, controller-manager Deployment (swiftletd runs in SwiftGuest pods created by controller; no DaemonSet)
- Add kustomizations for reproducible install
- Add Makefile targets: build-images, deploy, undeploy, load-images (for local dev)

## Capabilities

### New Capabilities

- `kubeswift-cluster-deployment`: Reproducible deployment packaging. Controller-manager and swiftletd image builds; namespace, service accounts, controller-manager Deployment; kustomizations; Makefile targets. Swiftletd image used by SwiftGuest pods; deployment path sufficient for smoke test as consumer.

### Modified Capabilities

- (none)

## Impact

- **Paths:** `images/controller-manager/Containerfile`, `config/deploy/base/` (manifests, kustomization), `Makefile`
- **Out of scope:** Smoke test changes, release publishing, migration, snapshots, new runtime features, CRD semantic changes
