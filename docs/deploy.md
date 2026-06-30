# KubeSwift Deployment

> **Note:** This document is retained for reference. For the canonical install docs, see [docs/index.md](index.md) → [Install](install/):
> - [Local cluster](install/local-cluster.md)
> - [Remote cluster](install/remote-cluster.md)
> - [Helm OCI](install/helm-oci.md)

This document describes the minimal install flow: build images, deploy KubeSwift to a Kubernetes cluster, and undeploy. The minimal path does not include admission webhooks; it is sufficient for first-boot smoke testing.

## Install from OCI (remote clusters)

For remote clusters, install KubeSwift from the OCI Helm chart:

### Prerequisites

- Helm 3+
- `kubectl` configured for the cluster
- Worker nodes with [worker-node preflight](operator/worker-node-preflight.md) passing (for SwiftGuest workloads)

### Install command

```bash
helm install kubeswift oci://ghcr.io/kubeswift-io/charts/kubeswift --version <version> -n kubeswift-system --create-namespace
```

### Version selection

| Release type | Chart version | Example |
|--------------|---------------|---------|
| Dev (main branch) | `0.0.0-dev.<shortsha>` | `0.0.0-dev.a1b2c3d` |
| Release candidate | `X.Y.Z-rc.N` | `0.1.0-rc.1` |
| Stable | `X.Y.Z` | `0.1.0` |

### Optional webhook

To enable admission webhooks (requires [cert-manager](https://cert-manager.io/)):

```bash
helm install kubeswift oci://ghcr.io/kubeswift-io/charts/kubeswift --version <version> -n kubeswift-system --create-namespace --set webhook.enabled=true
```

### Image overrides

For air-gapped or custom registry installs:

```bash
helm install kubeswift oci://ghcr.io/kubeswift-io/charts/kubeswift --version <version> -n kubeswift-system --create-namespace \
  --set controllerManager.image.registry=my-registry.io \
  --set controllerManager.image.tag=v0.1.0 \
  --set swiftletd.image.registry=my-registry.io \
  --set swiftletd.image.tag=v0.1.0
```

### Release workflow

| Trigger | Workflow | Image tag | Chart version |
|---------|----------|-----------|---------------|
| Push to `main` | `release-dev` | `sha-<shortsha>` | `0.0.0-dev.<shortsha>` |
| Tag `v*.*.*-rc.*` | `release-rc` | `vX.Y.Z-rc.N` | `X.Y.Z-rc.N` |
| Tag `v*.*.*` (no `-rc`) | `release-stable` | `vX.Y.Z` | `X.Y.Z` |

See [releases.md](releases.md) for version stamping, Makefile targets, and release details. See [install/helm-oci.md](install/helm-oci.md) for the canonical Helm OCI doc.

---

## Prerequisites (local)

- Kubernetes cluster (1.28+)
- `kubectl` configured for the cluster
- Docker (or compatible) for building images
- Worker nodes with [worker-node preflight](operator/worker-node-preflight.md) passing (for SwiftGuest workloads)

## Repository layout

```
images/
  controller-manager/Containerfile
  swiftletd/Containerfile

config/
  namespace/       # kubeswift-system namespace
  manager/         # controller-manager Deployment, RBAC, ServiceAccount, webhook Service
  daemonset/       # swiftletd DaemonSet
  default/         # install entrypoint (composes namespace, manager, daemonset)
  webhook/         # Certificate, Issuer, ValidatingWebhookConfiguration, MutatingWebhookConfiguration
  overlays/webhook/  # overlay to enable webhooks (requires cert-manager)
```

## Build images

Build controller-manager and swiftletd images:

```bash
make build-images
```

Or build individually:

```bash
make build-controller-image   # controller-manager
make build-swiftletd-image   # swiftletd
```

Images are tagged as `ghcr.io/kubeswift-io/kubeswift/<component>:latest` by default. Override with `IMAGE_TAG`:

```bash
make build-images IMAGE_TAG=v0.1.0
```

## Deploy

Deploy CRDs and KubeSwift components:

```bash
make deploy
```

This applies:

1. CRDs (`config/crd`)
2. Namespace, controller-manager, swiftletd DaemonSet (`config/default`)

### Local clusters (kind, minikube)

For local development, load built images into the cluster so it can pull them without a registry:

```bash
make load-images
make deploy
```

`load-images` detects `kind` or `minikube` and loads the controller-manager and swiftletd images.

## Undeploy

Remove KubeSwift from the cluster:

```bash
make undeploy
```

This deletes deployment resources first, then CRDs.

## Post-deploy: SwiftGuest workloads

After deploy, create SwiftGuests in a namespace. **Apply swiftletd RBAC in that namespace first:**

```bash
kubectl apply -k config/rbac -n <namespace>
```

Then create a SwiftGuest (see `config/samples/`). The smoke test (`make smoke-test`) expects RBAC to be applied in the target namespace.

## Override image tags

To deploy with custom image tags, use kustomize:

```bash
kubectl apply -k config/crd
kubectl kustomize config/default | \
  sed 's|ghcr.io/kubeswift-io/kubeswift/controller-manager:latest|your-registry/controller-manager:v1|g' | \
  kubectl apply -f -
```

Or create a kustomization overlay that sets `images` in `config/default`.

## Deploy with admission webhooks (optional)

The minimal path above does not enable admission webhooks. To enable validation and defaulting for SwiftGuest, SwiftImage, and SwiftSeedProfile:

### Prerequisites

- [cert-manager](https://cert-manager.io/) installed (v1.0+). For example:

  ```bash
  kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml
  ```

### Deploy flow

1. Apply CRDs and cert-manager (if not already installed).
2. Deploy KubeSwift with the webhook overlay:

   ```bash
   kubectl apply -k config/crd
   kubectl apply -k config/overlays/webhook
   ```

   The overlay adds: cert-manager Certificate/Issuer, ValidatingWebhookConfiguration, MutatingWebhookConfiguration, and patches the controller-manager to run with `--webhook-enabled=true` and mounted TLS certs. The webhook Service is in config/manager and deployed with the minimal path.

### Rollback

If webhooks block create/update (e.g. webhook unreachable), remove the webhook configurations and redeploy minimal:

```bash
kubectl delete validatingwebhookconfiguration kubeswift-validating-webhook --ignore-not-found
kubectl delete mutatingwebhookconfiguration kubeswift-mutating-webhook --ignore-not-found
kubectl apply -k config/default
```

## Notes

- **Minimal install**: No admission webhooks (ValidatingWebhookConfiguration, MutatingWebhookConfiguration). The controller-manager runs webhooks in-process but the API server does not call them; create/update succeeds without admission.
- **controller-manager**: Runs SwiftImage and SwiftGuest controllers.
- **swiftletd DaemonSet**: Runs swiftletd on each node. The SwiftGuest controller also creates pods with swiftletd as the launcher container.
- **swiftletd RBAC**: Apply `config/rbac` in each namespace where SwiftGuests run so swiftletd can patch SwiftGuest status.
