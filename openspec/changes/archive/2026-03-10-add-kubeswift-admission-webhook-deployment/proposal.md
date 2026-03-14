## Why

KubeSwift has webhook defaulters and validators in the repository (`internal/webhook/swiftguest`, `internal/webhook/swiftimage`, `internal/webhook/swiftseedprofile`) and the controller-manager registers them in-process. However, the deploy path does not yet expose them through Kubernetes admission resources and TLS configuration. The API server never calls the webhooks because ValidatingWebhookConfiguration and MutatingWebhookConfiguration are absent. Enabling admission webhook deployment is the natural follow-up after minimal cluster deployability is working—it activates validation and defaulting that already exists in code.

## What Changes

- Add webhook Service for controller-manager (expose webhook port)
- Add ValidatingWebhookConfiguration for SwiftGuest, SwiftImage, SwiftSeedProfile
- Add MutatingWebhookConfiguration for same resources
- Add TLS certificate handling for webhook serving (cert-manager or equivalent)
- Modify controller-manager Deployment and startup config to serve webhooks on the cluster (port, cert-dir, host)

**Out of scope:**

- Smoke-test redesign
- Release pipeline
- New runtime features

## Capabilities

### New Capabilities

- `kubeswift-admission-webhook-deployment`: Defines the deployment path for admission webhooks—Service, ValidatingWebhookConfiguration, MutatingWebhookConfiguration, TLS certificate handling, and controller-manager configuration—so the API server can call webhooks for SwiftGuest, SwiftImage, SwiftSeedProfile.

### Modified Capabilities

- None. The api-validation-and-defaulting spec defines webhook behavior; this change adds deployment plumbing. The minimal-kubeswift-cluster-deployment spec (or equivalent) may be extended to optionally include webhook resources; that is an overlay or composition choice, not a requirement change to the minimal spec.

## Impact

- **Paths:** `config/manager/` (Service, webhook configs, certificate, deployment patches), `cmd/controller-manager/main.go` (webhook port, cert-dir)
- **Dependencies:** cert-manager (or manual cert provisioning) for TLS
- **First-boot path:** Must remain stable; webhook deployment is additive (overlay or separate kustomization)
- **Risks:** Webhook TLS misconfiguration can block create/update; failure policy and documentation mitigate
- **Rollback:** Remove webhook configs; API server stops calling webhooks; create/update succeeds without admission
