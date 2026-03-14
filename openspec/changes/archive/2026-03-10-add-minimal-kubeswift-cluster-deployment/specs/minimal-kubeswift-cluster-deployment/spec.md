# Minimal KubeSwift Cluster Deployment Specification

## ADDED Requirements

### Requirement: Container build definitions for minimal deployment

The repository SHALL provide container build definitions for controller-manager and swiftletd. The controller-manager image SHALL be built from `images/controller-manager/Containerfile`. The swiftletd image SHALL use `images/swiftletd/Containerfile`. No separate webhook image is required for minimal deployment.

#### Scenario: Build controller-manager image

- **WHEN** operator runs the image build for controller-manager
- **THEN** the image is built from `cmd/controller-manager` via `images/controller-manager/Containerfile`
- **AND** the image is tagged as `ghcr.io/projectbeskar/kubeswift/controller-manager:latest` (or overridable)

#### Scenario: Build swiftletd image

- **WHEN** operator runs the image build for swiftletd
- **THEN** `images/swiftletd/Containerfile` is used
- **AND** the image is tagged as `ghcr.io/projectbeskar/kubeswift/swiftletd:latest` (per LauncherImage constant)

#### Scenario: Build all minimal images via Makefile

- **WHEN** operator runs `make build-images` for minimal deployment
- **THEN** controller-manager and swiftletd images are built
- **AND** both use repository-relative paths

### Requirement: Kubernetes manifests for minimal cluster install

The repository SHALL provide manifests at `config/namespace/`, `config/manager/`, `config/daemonset/`. Manifests SHALL include: namespace `kubeswift-system`, service accounts for controller-manager and swiftletd, controller-manager RBAC (ClusterRole, ClusterRoleBinding), controller-manager Deployment, swiftletd DaemonSet. Manifests SHALL be sufficient to install KubeSwift into a cluster for first-boot smoke testing. Manifests SHALL NOT include ValidatingWebhookConfiguration, MutatingWebhookConfiguration, or webhook TLS resources.

#### Scenario: Deploy creates namespace and controller-manager

- **WHEN** operator applies the deployment manifests
- **THEN** namespace `kubeswift-system` is created
- **AND** the controller-manager Deployment runs (controllers and in-process webhooks; API server does not call webhooks)

#### Scenario: Deploy creates swiftletd DaemonSet

- **WHEN** operator applies the deployment manifests
- **THEN** the swiftletd DaemonSet is created
- **AND** swiftletd pods run on each node with privileged access and host paths for `/var/lib/kubeswift` and `/dev/kvm`

#### Scenario: Controller creates SwiftGuest pods with swiftletd image

- **WHEN** operator creates a SwiftGuest after deploy
- **THEN** the controller creates a Pod with launcher container using the swiftletd image
- **AND** the swiftletd image must be built and available (per LauncherImage constant)

### Requirement: Install kustomization

The repository SHALL provide `config/default/kustomization.yaml` that composes namespace, manager, and daemonset. The install path SHALL be `kubectl apply -k config/default` after CRDs are applied. The minimal install SHALL NOT include webhook Deployment or Service.

#### Scenario: Install via kustomize

- **WHEN** operator runs `kubectl apply -k config/default`
- **THEN** namespace, manager, and daemonset resources are applied
- **AND** image tags can be overridden via kustomize `images`

### Requirement: Makefile targets

The repository SHALL provide: `make build-images`, `make deploy`, `make undeploy`, `make load-images`. Deploy SHALL apply CRDs first, then deploy base. Undeploy SHALL remove deploy resources first, then CRDs. Load-images SHALL load controller-manager and swiftletd images into kind/minikube for local development.

#### Scenario: Deploy to cluster

- **WHEN** operator runs `make deploy`
- **THEN** CRDs are applied, then `kubectl apply -k config/default`
- **AND** KubeSwift is installed for minimal deployment

#### Scenario: Undeploy from cluster

- **WHEN** operator runs `make undeploy`
- **THEN** `kubectl delete -k config/default`, then CRDs are removed
- **AND** the cluster no longer has KubeSwift components

#### Scenario: Load images for local development

- **WHEN** operator runs `make load-images` with a local cluster (kind/minikube)
- **THEN** controller-manager and swiftletd images are loaded into the cluster runtime
- **AND** deploy can use local images without a registry

### Requirement: Deployment documentation

The repository SHALL provide `docs/deploy.md` that documents the minimal install flow and smoke-test preparation. Documentation SHALL include: build images, deploy, undeploy, load-images for local clusters, and post-deploy steps (apply RBAC in SwiftGuest namespace).

#### Scenario: Operator follows deploy docs

- **WHEN** operator reads `docs/deploy.md` and follows the deploy steps
- **THEN** they can build images, deploy to cluster, and prepare for smoke test
- **AND** the resulting cluster satisfies first-boot smoke test prerequisites

### Requirement: Smoke test compatibility

The resulting deploy path SHALL be sufficient to support the existing boot smoke test (`test/smoke/boot-test.sh`). The smoke test prerequisites (CRDs, controllers, swiftletd image, RBAC in target namespace) SHALL be satisfiable after minimal deploy.

#### Scenario: Smoke test runs after minimal deploy

- **WHEN** operator runs `make deploy`, applies RBAC in target namespace, and runs `make smoke-test`
- **THEN** the smoke test can proceed (subject to node readiness, image URL, etc.)
- **AND** no admission webhook configuration is required
