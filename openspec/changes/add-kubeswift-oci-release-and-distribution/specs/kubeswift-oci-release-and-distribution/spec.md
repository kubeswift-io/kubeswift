# KubeSwift OCI Release and Distribution Specification

## ADDED Requirements

### Requirement: OCI images in GHCR as sole image distribution

The repository SHALL publish controller-manager and swiftletd images to GitHub Container Registry. Image names SHALL be exactly: `ghcr.io/projectbeskar/kubeswift/controller-manager`, `ghcr.io/projectbeskar/kubeswift/swiftletd`. Webhooks are served by the controller-manager; no separate webhook image SHALL be published. Tags SHALL be immutable: dev `sha-<short-sha>`, RC `vX.Y.Z-rc.N`, stable `vX.Y.Z`. No alternate registries or distribution formats for published releases.

#### Scenario: Dev image published on push to main

- **WHEN** a push to `main` triggers the dev workflow
- **THEN** controller-manager and swiftletd images are built and pushed to GHCR
- **AND** each image is tagged with `sha-<short-sha>` (e.g. `sha-a1b2c3d`)

#### Scenario: RC image published on tag push

- **WHEN** a tag matching `v*.*.*-rc.*` is pushed
- **THEN** images are built and pushed with the tag as the image tag (e.g. `v0.1.0-rc.1`)
- **AND** the tag is immutable

#### Scenario: Stable image published on tag push

- **WHEN** a tag matching `v*.*.*` (without `-rc`) is pushed
- **THEN** images are built and pushed with the tag (e.g. `v0.1.0`)
- **AND** a GitHub Release is created

### Requirement: OCI Helm chart in GHCR as primary install artifact

The repository SHALL provide an installable Helm chart named `kubeswift` at OCI location `oci://ghcr.io/projectbeskar/charts/kubeswift`. The chart SHALL be the primary supported install artifact for remote clusters. The chart SHALL package CRDs, controller-manager, swiftletd, and optionally webhook resources. No competing install formats (raw YAML releases, alternate chart repos) SHALL be supported.

#### Scenario: Chart install from OCI

- **WHEN** operator runs `helm install kubeswift oci://ghcr.io/projectbeskar/charts/kubeswift --version X.Y.Z`
- **THEN** KubeSwift is installed with the specified version
- **AND** image tags in the chart default to the release version

#### Scenario: No competing install path

- **WHEN** operator installs KubeSwift on a remote cluster
- **THEN** the supported path is the OCI Helm chart from GHCR
- **AND** raw manifest releases or alternate chart repositories are not provided

#### Scenario: Chart supports image overrides

- **WHEN** operator installs with `--set controllerManager.image.registry=my-registry.io`
- **THEN** the chart uses the overridden registry for controller-manager
- **AND** air-gapped or custom registry installs are supported

### Requirement: Chart derived from config/ manifests

The Helm chart templates SHALL be derived from or aligned with existing `config/` manifests. The chart SHALL include: optionally namespace (or document `helm install --create-namespace`), controller-manager (Deployment with webhook conditionals inlined, Service, RBAC, ServiceAccount), swiftletd DaemonSet, and optionally webhook resources (Certificate, Issuer, ValidatingWebhook, MutatingWebhook). Webhook conditionals SHALL be in the controller-manager deployment template, not in a separate deployment-patch. CRDs SHALL be included from `config/crd/bases/`.

#### Scenario: Chart deploys equivalent to config/default

- **WHEN** operator installs the chart with default values
- **THEN** the resulting resources are equivalent to `kubectl apply -k config/default` with image tags set to the chart version
- **AND** CRDs are applied from the chart

### Requirement: GitHub Actions workflow responsibilities (exact)

The repository SHALL provide exactly three publishing workflows: `.github/workflows/publish-dev.yaml` (trigger: push to main; builds and pushes controller-manager and swiftletd with `sha-<short-sha>`, chart with `0.0.0-dev.<short-sha>` to `oci://ghcr.io/projectbeskar/charts/kubeswift`), `.github/workflows/publish-rc.yaml` (trigger: tag `v*.*.*-rc.*`; builds and pushes images and chart with version from tag), `.github/workflows/publish-release.yaml` (trigger: tag `v*.*.*` excluding `-rc`; builds and pushes images and chart; creates GitHub Release). No other workflows SHALL publish to GHCR.

#### Scenario: Dev workflow on push to main

- **WHEN** code is pushed to `main`
- **THEN** the dev workflow builds and pushes images with `sha-<short-sha>` tags
- **AND** the chart is built and pushed with version `0.0.0-dev.<short-sha>` to `oci://ghcr.io/projectbeskar/charts/kubeswift`

#### Scenario: Stable workflow creates GitHub Release

- **WHEN** a tag `vX.Y.Z` is pushed
- **THEN** images and chart are published
- **AND** a GitHub Release is created with the tag

### Requirement: Local workflows preserved

The repository SHALL NOT remove or replace `make build-images`, `make deploy`, or `make load-images`. Local-cluster workflows SHALL remain functional for kind/minikube development.

#### Scenario: Local build and load unchanged

- **WHEN** developer runs `make build-images` and `make load-images`
- **THEN** images are built and loaded into kind/minikube as before
- **AND** `make deploy` continues to work for local clusters
