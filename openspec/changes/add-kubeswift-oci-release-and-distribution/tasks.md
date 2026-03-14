## 1. Helm Chart Scaffold

- [x] 1.1 Create `charts/kubeswift/Chart.yaml` with name, version, appVersion
- [x] 1.2 Create `charts/kubeswift/values.yaml` with controllerManager, swiftletd image config (registry, tag); webhook.enabled for optional webhook mode
- [x] 1.3 Create `charts/kubeswift/.helmignore` and directory structure (templates/, crds/)

## 2. Chart Templates from config/

- [x] 2.1 Add `charts/kubeswift/crds/` with CRDs from `config/crd/bases/`
- [x] 2.2 Add optional `charts/kubeswift/templates/namespace.yaml` from config/namespace (or document `helm install --create-namespace`)
- [x] 2.3 Add controller-manager templates (deployment, service, serviceaccount, rbac) from config/manager; put webhook conditionals directly in deployment template (no deployment-patch.yaml)
- [x] 2.4 Add swiftletd templates (daemonset, serviceaccount) from config/daemonset
- [x] 2.5 Wire image values into deployment and daemonset templates
- [x] 2.6 Add optional webhook conditional templates from config/webhook (Certificate, Issuer, ValidatingWebhook, MutatingWebhook); webhook Service is in config/manager

## 3. Dev Image Publishing Workflow

- [x] 3.1 Add `.github/workflows/publish-dev.yaml` triggered on push to main
- [x] 3.2 Build and push controller-manager, swiftletd with tag `sha-<short-sha>`
- [x] 3.3 Build Helm chart with version `0.0.0-dev.<short-sha>` and push to `oci://ghcr.io/projectbeskar/charts/kubeswift`

## 4. Release-Candidate Workflow

- [x] 4.1 Add `.github/workflows/publish-rc.yaml` triggered on tag `v*.*.*-rc.*`
- [x] 4.2 Build and push controller-manager, swiftletd with tag matching the git tag (e.g. v0.1.0-rc.1)
- [x] 4.3 Build and push Helm chart with version matching tag

## 5. Stable Release Workflow

- [x] 5.1 Add `.github/workflows/publish-release.yaml` triggered on tag `v*.*.*` (excluding -rc)
- [x] 5.2 Build and push controller-manager, swiftletd with tag matching the git tag
- [x] 5.3 Build and push Helm chart with version matching tag
- [x] 5.4 Create GitHub Release with tag, release notes, and install instructions

## 6. Documentation

- [x] 6.1 Document OCI install in docs/deploy.md: `helm install kubeswift oci://ghcr.io/projectbeskar/charts/kubeswift --version <version> --create-namespace`, version selection
- [x] 6.2 Document dev/RC/stable workflow triggers and tag conventions in README or docs
- [x] 6.3 Update docs/repo-layout.md: add charts/kubeswift/, OCI chart reference, clarify webhook served by controller-manager
