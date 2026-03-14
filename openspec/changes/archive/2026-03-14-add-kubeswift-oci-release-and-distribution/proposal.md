## Why

KubeSwift can now be built and installed locally via `make build-images`, `make deploy`, and `make load-images` for kind/minikube. Remote test clusters and production deployments require published artifacts that can be pulled from a registry. Without OCI-native release and distribution, operators cannot install KubeSwift on clusters that lack local image builds. The project needs release plumbing for dev releases (branch pushes), release candidates (tags like `vX.Y.Z-rc.N`), and stable releases (tags like `vX.Y.Z`). OCI-native distribution—container images and Helm charts published to GitHub Container Registry (GHCR)—is the standard model for Kubernetes operators and aligns with KubeSwift's Cloud-Hypervisor-native, Kubernetes-native architecture.

## What Changes

- Publish controller-manager and swiftletd images to GHCR (`ghcr.io/projectbeskar/kubeswift/*`); webhooks are served by controller-manager, no separate webhook image
- Add a versioning strategy for dev, release-candidate, and stable releases
- Add an installable Helm chart for KubeSwift at `oci://ghcr.io/projectbeskar/charts/kubeswift`
- Publish the Helm chart to GHCR as an OCI artifact
- Add GitHub Actions workflows for: dev image publishing, release-candidate publishing, stable release publishing
- Wire image tags and chart versions to the chosen versioning model
- Keep local-cluster workflows (make load-images, make deploy) intact

**Out of scope:**

- Migration, snapshots, or new runtime features
- Changing CRD semantics except where release packaging requires it
- Replacing local-cluster workflows
- Adding a second distribution format; OCI images + OCI Helm chart in GHCR is the sole remote install path

## Capabilities

### New Capabilities

- `kubeswift-oci-release-and-distribution`: Defines OCI-native release and distribution—image naming and tag conventions, Helm chart packaging and OCI publishing, GitHub Actions workflows for dev/RC/stable, and how existing config/ manifests map into the chart.

### Modified Capabilities

- None. Local deploy and load-images remain unchanged; this change adds remote install paths.

## Impact

- **Paths:** `.github/workflows/` (new workflows), `charts/kubeswift/` (new Helm chart), `config/` (referenced by chart templates)
- **Chart:** Webhook conditionals in controller-manager deployment template; namespace optional (use `helm install --create-namespace`)
- **Registry:** `ghcr.io/projectbeskar/kubeswift` for images; `oci://ghcr.io/projectbeskar/charts/kubeswift` for chart OCI
- **Dependencies:** Helm 3+ for chart install; GitHub Actions for CI/CD
- **Risks:** Registry auth, tag immutability, chart version drift
- **Rollback:** Revert workflows; local workflows unaffected
