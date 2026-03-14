# KubeSwift Cluster Deployment Specification

## ADDED Requirements

### Requirement: Container build definitions for control plane and node runtime

The repository SHALL provide container build definitions for controller-manager and swiftletd. The controller-manager image SHALL be built from `images/controller-manager/Containerfile`. The swiftletd image SHALL use the existing `images/swiftletd/Containerfile`. Webhooks run in-process in the controller-manager; no separate webhook image.

#### Scenario: Build controller-manager image

- **WHEN** operator runs the image build for controller-manager
- **THEN** the image is built from `cmd/controller-manager` via `images/controller-manager/Containerfile`
- **AND** the image is tagged as `ghcr.io/projectbeskar/kubeswift/controller-manager:latest` (or overridable)

#### Scenario: Build swiftletd image

- **WHEN** operator runs the image build for swiftletd
- **THEN** the existing `images/swiftletd/Containerfile` is used
- **AND** the image is tagged as `ghcr.io/projectbeskar/kubeswift/swiftletd:latest` (per LauncherImage constant)

#### Scenario: Build all images via Makefile

- **WHEN** operator runs `make build-images`
- **THEN** controller-manager and swiftletd images are built
- **AND** both use repository-relative paths

### Requirement: Kubernetes manifests for cluster install

The repository SHALL provide manifests at `config/deploy/base/`: namespace (`namespace.yaml`), service accounts (`serviceaccount.yaml`), controller-manager RBAC (`controller-manager-rbac.yaml`), controller-manager Deployment (`controller-manager.yaml`). Swiftletd runs in SwiftGuest pods created by the controller; no DaemonSet. Manifests SHALL be sufficient to install KubeSwift into a cluster.

#### Scenario: Deploy creates namespace and controller-manager

- **WHEN** operator applies the deployment manifests
- **THEN** namespace `kubeswift-system` is created
- **AND** the controller-manager Deployment runs (controllers and webhooks in-process)

#### Scenario: Controller creates SwiftGuest pods with swiftletd image

- **WHEN** operator creates a SwiftGuest after deploy
- **THEN** the controller creates a Pod with launcher container using the swiftletd image
- **AND** the swiftletd image must be built and available (per LauncherImage constant)

### Requirement: Install kustomization

The repository SHALL provide `config/deploy/base/kustomization.yaml` that composes namespace, serviceaccount, controller-manager-rbac, and controller-manager. The install path SHALL be `kubectl apply -k config/deploy/base` after CRDs are applied.

#### Scenario: Install via kustomize

- **WHEN** operator runs `kubectl apply -k config/deploy/base`
- **THEN** all deployment resources are applied
- **AND** image tags can be overridden via kustomize `images`

### Requirement: Makefile targets

The repository SHALL provide: `make build-images`, `make deploy`, `make undeploy`, `make load-images`. Deploy SHALL apply CRDs first, then deploy base. Undeploy SHALL remove deploy resources first, then CRDs. Load-images SHALL load built images into kind/minikube for local development.

#### Scenario: Deploy to cluster

- **WHEN** operator runs `make deploy`
- **THEN** CRDs are applied, then `kubectl apply -k config/deploy/base`
- **AND** KubeSwift is installed

#### Scenario: Undeploy from cluster

- **WHEN** operator runs `make undeploy`
- **THEN** `kubectl delete -k config/deploy/base`, then CRDs are removed
- **AND** the cluster no longer has KubeSwift components

#### Scenario: Load images for local development

- **WHEN** operator runs `make load-images` with a local cluster (kind/minikube)
- **THEN** controller-manager and swiftletd images are loaded into the cluster runtime
- **AND** deploy can use local images without a registry

### Requirement: Design consistent with existing code

The deployment design SHALL be consistent with existing code: controller-manager runs controllers and webhooks in one process (`cmd/controller-manager/main.go`); swiftletd image name per `internal/controller/swiftguest/constants.go` LauncherImage; config/rbac for swiftletd namespace-scoped RBAC remains separate.

#### Scenario: No separate webhook deployment

- **WHEN** operator inspects the deployment
- **THEN** there is no separate webhook Deployment or webhook image
- **AND** webhooks are served by the controller-manager Deployment
