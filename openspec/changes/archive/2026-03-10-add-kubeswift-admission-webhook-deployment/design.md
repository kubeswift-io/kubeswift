## Context

The controller-manager (`cmd/controller-manager`) registers webhooks for SwiftGuest, SwiftImage, and SwiftSeedProfile via controller-runtime. Webhooks run in-process on the default port 9443. The minimal deploy path does not expose webhooks: no Service, no ValidatingWebhookConfiguration, no MutatingWebhookConfiguration, no TLS. The API server never calls the webhooks. This design adds the deployment plumbing so admission is activated.

## Goals / Non-Goals

**Goals:**

- Add webhook Service for controller-manager (expose port 9443)
- Add ValidatingWebhookConfiguration and MutatingWebhookConfiguration for SwiftGuest, SwiftImage, SwiftSeedProfile
- Add TLS certificate handling (cert-manager Certificate + CA injection)
- Modify controller-manager Deployment (port, cert volume) and main.go (cert-dir, host, port)
- Document cert-manager prerequisite clearly
- Keep the first-boot minimal path stable (webhook resources as overlay or optional inclusion)

**Non-Goals:**

- Smoke-test redesign
- Release pipeline
- New runtime features
- Separate webhook server (webhooks stay in controller-manager)

## Decisions

### 1. Webhook Service targets controller-manager

**Decision:** Add a Service named `kubeswift-webhook-service` (or `controller-manager`-scoped) that selects the controller-manager pods and exposes port 9443. The ValidatingWebhookConfiguration and MutatingWebhookConfiguration reference this Service.

**Rationale:** Webhooks run in-process in the controller-manager; no separate webhook Deployment. The Service must point to the controller-manager Deployment.

**Alternative considered:** Separate webhook-server Deployment. Rejected—webhook-server is a stub; webhooks are in controller-manager.

### 2. TLS via cert-manager

**Decision:** Use cert-manager Certificate resource to provision TLS certs for the webhook Service. Mount the resulting Secret at the cert-dir expected by controller-runtime. Use cert-manager CA injector (`cert-manager.io/inject-ca-from` annotation) to populate `caBundle` in webhook configs.

**Rationale:** Standard Kubernetes pattern; avoids manual cert management. cert-manager is widely used and well-documented.

**Alternative considered:** controller-runtime self-signed cert generation. Rejected for production; acceptable for dev but adds code paths. cert-manager is the recommended approach.

### 3. Webhook config layout

**Decision:** Place ValidatingWebhookConfiguration, MutatingWebhookConfiguration, Certificate, and Service in `config/manager/` (or a `config/webhook/` subdir that patches manager). Use kustomize to compose. The install path may be `config/default` with webhook resources added, or a separate overlay `config/overlays/webhook`.

**Rationale:** Keeps webhook deployment composable; minimal install can omit it. Overlay approach preserves first-boot stability.

### 4. Controller-manager webhook options

**Decision:** Configure `ctrl.Options` in `cmd/controller-manager/main.go` with `Port: 9443`, `Host: "0.0.0.0"`, and `CertDir` from env or flag (e.g. `/tmp/k8s-webhook-server/serving-certs`). Mount cert-manager Secret at that path in the Deployment.

**Rationale:** controller-runtime expects certs at a configurable path; cert-manager writes to a Secret we mount.

### 5. Failure policy

**Decision:** Use `failurePolicy: Fail` for validation webhooks (reject on webhook failure) and `failurePolicy: Ignore` for mutating webhooks (allow on failure, defaults not applied). Document that webhook unavailability blocks create/update when Fail.

**Rationale:** Fail for validation ensures invalid resources are rejected. Ignore for mutation avoids blocking on transient webhook issues; operators can retry.

**Alternative:** Fail for both. Stricter but more operational burden.

## Risks / Trade-offs

| Risk | Mitigation |
|------|------------|
| cert-manager not installed blocks deploy | Document prerequisite; provide clear error message or preflight check |
| Webhook TLS misconfiguration blocks all create/update | Document rollback: delete ValidatingWebhookConfiguration, MutatingWebhookConfiguration |
| First-boot path regresses | Webhook resources in separate overlay; minimal `config/default` unchanged |
| CA bundle injection timing | cert-manager CA injector runs asynchronously; document wait-for-ready if needed |

## Migration Plan

1. Add cert-manager as documented prerequisite
2. Add Service, Certificate, ValidatingWebhookConfiguration, MutatingWebhookConfiguration
3. Patch controller-manager Deployment (port, cert volume)
4. Update cmd/controller-manager main.go (webhook options)
5. Add overlay or include webhook resources in config/default (optional)
6. **Rollback:** Delete webhook configs; remove cert volume from Deployment; revert main.go if needed

## Open Questions

- None. Scope is well-defined.
