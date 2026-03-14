# KubeSwift Admission Webhook Deployment Specification

## ADDED Requirements

### Requirement: Webhook Service for controller-manager

The repository SHALL provide a Service that exposes the controller-manager webhook port. The Service SHALL select the controller-manager Deployment pods and expose port 9443 (or the port configured for controller-runtime webhook server). The Service SHALL be in namespace `kubeswift-system`.

#### Scenario: Service exposes webhook port

- **WHEN** operator applies the webhook deployment manifests
- **THEN** a Service exists that targets controller-manager pods
- **AND** the Service exposes the webhook port (9443)

#### Scenario: API server can reach webhook

- **WHEN** the API server receives a create/update for SwiftGuest, SwiftImage, or SwiftSeedProfile
- **THEN** it calls the webhook at the Service URL
- **AND** the controller-manager serves the admission request

### Requirement: ValidatingWebhookConfiguration

The repository SHALL provide a ValidatingWebhookConfiguration for SwiftGuest, SwiftImage, and SwiftSeedProfile. The webhook SHALL reference the webhook Service. The configuration SHALL include the correct `caBundle` (injected by cert-manager).

#### Scenario: Validation webhook registered

- **WHEN** operator applies the ValidatingWebhookConfiguration
- **THEN** the API server calls the webhook for create/update of swiftguests, swiftimages, swiftseedprofiles
- **AND** invalid requests are rejected by the webhook

#### Scenario: Validation failure policy

- **WHEN** the webhook is unavailable or returns an error
- **THEN** the API server rejects the request (failurePolicy: Fail) or allows it (configurable)
- **AND** the behavior is documented

### Requirement: MutatingWebhookConfiguration

The repository SHALL provide a MutatingWebhookConfiguration for SwiftGuest, SwiftImage, and SwiftSeedProfile. The webhook SHALL reference the webhook Service. The configuration SHALL include the correct `caBundle`.

#### Scenario: Mutation webhook registered

- **WHEN** operator applies the MutatingWebhookConfiguration
- **THEN** the API server calls the webhook for create/update of swiftguests, swiftimages, swiftseedprofiles
- **AND** defaults are applied before validation and persistence

### Requirement: TLS certificate handling (cert-manager only)

The repository SHALL provide TLS certificate handling for webhook serving via cert-manager only. The controller-manager SHALL serve webhooks over TLS. Certificates SHALL be provisioned via cert-manager (Certificate resource). controller-runtime self-signed or manual certificate approaches SHALL NOT be supported. The controller-manager Deployment SHALL mount the certificate Secret at the cert-dir expected by controller-runtime.

#### Scenario: cert-manager provisions certificate

- **WHEN** cert-manager is installed and a Certificate resource is applied for the webhook Service
- **THEN** cert-manager creates a Secret with tls.crt and tls.key
- **AND** the controller-manager mounts that Secret and serves TLS

#### Scenario: CA bundle in webhook configs

- **WHEN** cert-manager CA injector runs
- **THEN** the ValidatingWebhookConfiguration and MutatingWebhookConfiguration have a valid caBundle
- **AND** the API server trusts the webhook server certificate

#### Scenario: Minimal path has no TLS (insecure path for dev)

- **WHEN** operator uses minimal deploy (config/default, --webhook-enabled=false)
- **THEN** no webhooks are registered, no TLS is required
- **AND** create/update succeeds without admission enforcement (insecure path for development and smoke-test)

### Requirement: Controller-manager webhook configuration

The controller-manager SHALL be configurable to serve webhooks on a cluster-accessible port with TLS. The repository SHALL provide Deployment patches or configuration that: expose container port 9443, mount the certificate volume at the cert-dir, and set cert-dir (and optionally host, port) via args or env.

#### Scenario: Controller-manager serves webhooks

- **WHEN** the controller-manager starts with cert-dir and port configured
- **THEN** it serves webhooks on the configured port with TLS
- **AND** the Service routes traffic to that port

#### Scenario: Cert-dir configurable

- **WHEN** operator deploys with cert-manager
- **THEN** the cert-dir matches the mount path of the certificate Secret
- **AND** the path is configurable via env or flag

### Requirement: First-boot path stability

The webhook deployment SHALL NOT break the minimal first-boot deployment path. The minimal install (namespace, manager, daemonset without webhook resources) SHALL remain functional. Webhook resources SHALL be additive—via overlay, optional kustomization, or separate apply step.

#### Scenario: Minimal deploy unchanged

- **WHEN** operator runs the minimal deploy (no webhook overlay)
- **THEN** CRDs, controller-manager, swiftletd DaemonSet are applied
- **AND** create/update succeeds without admission (webhooks not called)

#### Scenario: Webhook deploy additive

- **WHEN** operator applies webhook resources (overlay or separate step)
- **THEN** ValidatingWebhookConfiguration, MutatingWebhookConfiguration, Service, Certificate are added
- **AND** admission is activated for SwiftGuest, SwiftImage, SwiftSeedProfile

### Requirement: cert-manager prerequisite documentation

The repository SHALL document the cert-manager prerequisite clearly for webhook deployment. Documentation SHALL include: install command or link, required version, and any namespace or configuration requirements. Documentation SHALL state that the minimal path (no webhooks) requires no cert-manager and is the insecure path for development and smoke-test.

#### Scenario: Operator installs cert-manager

- **WHEN** operator reads the deployment documentation
- **THEN** they find the cert-manager install command or link
- **AND** they can install cert-manager before applying webhook resources

#### Scenario: cert-manager not installed

- **WHEN** operator applies webhook resources without cert-manager
- **THEN** the Certificate resource is not fulfilled (or equivalent failure)
- **AND** the documentation explains the prerequisite
