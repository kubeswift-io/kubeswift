## Why

KubeSwift already contains webhook defaulters and validators in the repository (`internal/webhook/swiftguest`, `internal/webhook/swiftimage`, `internal/webhook/swiftseedprofile`), and the controller-manager registers them in-process. The minimal deployment path is implemented and working, but admission webhooks are not yet exposed through Kubernetes admission resources and TLS configuration. Without webhook deployment, the API server never calls the webhooks—validation and defaulting exist in code but are not enforced. Enabling admission webhook deployment is the natural follow-up after minimal cluster deployability: it activates validation and defaulting that already exist.

## What Changes

- Add a Service for the controller-manager webhook endpoint (expose port 9443)
- Add ValidatingWebhookConfiguration for SwiftGuest, SwiftImage, and SwiftSeedProfile
- Add MutatingWebhookConfiguration for SwiftGuest, SwiftImage, and SwiftSeedProfile
- Add TLS certificate handling for webhook serving via cert-manager (required; controller-runtime self-signed not supported)
- Modify controller-manager Deployment and startup config to serve webhooks on the cluster (port, cert-dir, host)
- Update deployment docs and Makefile targets only as needed to support webhook-enabled deployment

**Out of scope:**

- Smoke-test redesign
- Release publishing
- New runtime features

## Capabilities

### New Capabilities

- `kubeswift-admission-webhook-deployment`: Defines the deployment path for admission webhooks—Service, ValidatingWebhookConfiguration, MutatingWebhookConfiguration, TLS certificate handling, and controller-manager configuration—so the API server can call webhooks for SwiftGuest, SwiftImage, SwiftSeedProfile.

### Modified Capabilities

- None. The api-validation-and-defaulting spec defines webhook behavior; this change adds deployment plumbing. The minimal deploy path remains unchanged.

## Impact

- **Paths:** `config/webhook/` (Service, Certificate, ValidatingWebhookConfiguration, MutatingWebhookConfiguration), `config/overlays/webhook/` (overlay for webhook-enabled deploy), `config/manager/deployment.yaml` (containerPort 9443), `cmd/controller-manager/main.go` (webhook port, cert-dir, host)
- **Dependencies:** cert-manager for TLS (required for webhook path; minimal path has no TLS)
- **First-boot path:** Must remain stable; webhook deployment is additive (overlay or separate kustomization)
- **Risks:** Webhook TLS misconfiguration or pod unavailability can block create/update; failure policy and documentation mitigate
- **Rollback:** Remove ValidatingWebhookConfiguration and MutatingWebhookConfiguration; API server stops calling webhooks; create/update succeeds without admission
