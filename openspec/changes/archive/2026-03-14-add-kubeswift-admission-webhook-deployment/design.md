## Context

The controller-manager (`cmd/controller-manager`) registers webhooks for SwiftGuest, SwiftImage, and SwiftSeedProfile via controller-runtime. Webhooks run in-process on port 9443. The minimal deploy path does not expose webhooks: no Service, no ValidatingWebhookConfiguration, no MutatingWebhookConfiguration, no TLS. The API server never calls the webhooks. This design adds the deployment plumbing so admission is activated.

## Goals / Non-Goals

**Goals:**

- Add webhook Service for controller-manager (expose port 9443)
- Add ValidatingWebhookConfiguration and MutatingWebhookConfiguration for SwiftGuest, SwiftImage, SwiftSeedProfile
- Add TLS certificate handling (cert-manager Certificate + CA injection)
- Modify controller-manager Deployment (port, cert volume) and main.go (cert-dir, host, port)
- Document cert-manager prerequisite clearly (cert-manager is required; no self-signed bootstrapping in this design)
- Keep the first-boot minimal path stable (webhook resources as overlay or optional inclusion)
- Ensure webhook deployment is included in the cluster install path (via overlay)
- Keep the install path reproducible

**Non-Goals:**

- Smoke-test redesign
- Release pipeline
- New runtime features
- Separate webhook server (webhooks stay in controller-manager)
- controller-runtime self-signed or manual certificate approach (rejected; see TLS decision)

## TLS Strategy Decision

**Chosen: cert-manager-backed certificates only.** controller-runtime self-signed/manual approach is not supported.

**Justification:**

| Criterion | cert-manager | controller-runtime self-signed |
|-----------|--------------|-------------------------------|
| Simplicity for dev/smoke-test | One prerequisite (cert-manager); self-signed ClusterIssuer = no external CA | Would require custom CA injection code to patch webhook configs; controller-runtime does not provide this |
| Reproducible install | Same sequence every time: install cert-manager → apply overlay | Custom logic adds failure modes and version drift |
| Operator confusion | Single path: cert-manager required for webhooks | Two paths (cert-manager vs self-signed) = half-supported, confusing |
| Insecure path for quick dev | Minimal deploy (no webhooks) = no TLS, no admission; use `config/default` | Same |

**Insecure path:** For development and smoke-test without admission enforcement, use the minimal deploy path (`kubectl apply -k config/default`). No webhooks, no TLS, no cert-manager. This is the "insecure" option—admission is not enforced. When webhooks are required, cert-manager is required; there is no "insecure flag" for webhook TLS (the API server does not support it).

## Decisions

### 1. Webhook Service targets controller-manager

**Decision:** Add a Service named `kubeswift-webhook-service` that selects the controller-manager pods and exposes port 9443. The ValidatingWebhookConfiguration and MutatingWebhookConfiguration reference this Service.

**Rationale:** Webhooks run in-process in the controller-manager; no separate webhook Deployment. The Service must point to the controller-manager Deployment.

**How the API server reaches the webhook:** The API server resolves the Service DNS name (`kubeswift-webhook-service.kubeswift-system.svc`) and connects to port 443 (Service port; targetPort 9443). The webhook configs specify `path` for each webhook (e.g. `/validate-swift-kubeswift-io-v1alpha1-swiftguest`). Traffic flows: API server → Service (ClusterIP) → controller-manager pod port 9443.

### 2. TLS via cert-manager (required; only supported approach)

**Decision:** Use cert-manager Certificate resource to provision TLS certs for the webhook Service. Mount the resulting Secret at the cert-dir expected by controller-runtime. Use cert-manager CA injector (`cert-manager.io/inject-ca-from` annotation) to populate `caBundle` in webhook configs. **cert-manager is required for webhook deployment.** controller-runtime self-signed or manual certificate approaches are not supported.

**Where webhook TLS certificates come from:** cert-manager issues a Certificate for the webhook Service DNS name via a self-signed ClusterIssuer (no external CA). It creates a Secret (`kubeswift-webhook-cert`) with `tls.crt` and `tls.key`. The controller-manager mounts this Secret at `/tmp/k8s-webhook-server/serving-certs` (or configurable path). Production can swap the Issuer for a proper CA.

**Rationale:** cert-manager handles both cert issuance and CA injection; no custom code. controller-runtime self-signed would require custom logic to extract CA and patch webhook configs—more code, more failure modes. Single approach minimizes operator confusion.

### 3. Webhook config layout

**Decision:** Place ValidatingWebhookConfiguration, MutatingWebhookConfiguration, Certificate, Issuer, and Service in `config/webhook/`. Use a separate overlay `config/overlays/webhook` that composes `config/default` + `config/webhook` and patches the controller-manager Deployment for webhook mode.

**Rationale:** Keeps webhook deployment composable; minimal install omits it. Overlay approach preserves first-boot stability.

### 4. Controller-manager webhook options

**Decision:** Add `--webhook-enabled` flag (default `false`) so minimal deploy runs without webhooks. When `--webhook-enabled=true`, configure `ctrl.Options` with `Port: 9443`, `Host: "0.0.0.0"`, and `CertDir` from env or flag (default `/tmp/k8s-webhook-server/serving-certs`). Mount cert-manager Secret at that path in the Deployment via overlay patch.

**Rationale:** controller-runtime expects certs at a configurable path; cert-manager writes to a Secret we mount. Minimal path stays unchanged by default.

### 5. Failure policy

**Decision:** Use `failurePolicy: Fail` for validation webhooks (reject on webhook failure) and `failurePolicy: Ignore` for mutating webhooks (allow on failure, defaults not applied). Document that webhook unavailability blocks create/update when Fail.

**Failure modes:**

| Condition | Effect | Mitigation |
|-----------|--------|------------|
| cert-manager not installed | Certificate resource not fulfilled; Secret not created; controller-manager cannot serve TLS | Document prerequisite; operator must install cert-manager first |
| Certificate not ready | controller-manager may fail to start or webhook server fails TLS handshake | Wait for cert-manager to issue cert; controller-manager retries or restarts |
| Webhook pods not ready | API server calls webhook; connection fails or times out | With `failurePolicy: Fail`: create/update rejected. Rollback: delete ValidatingWebhookConfiguration, MutatingWebhookConfiguration |
| CA bundle not injected | API server cannot verify webhook TLS cert; admission fails | cert-manager CA injector runs asynchronously; document wait-for-ready if needed |

**Rationale:** Fail for validation ensures invalid resources are rejected. Ignore for mutation avoids blocking on transient webhook issues; operators can retry.

### 6. Install wiring (reproducible path)

**Decision:** Minimal path: `kubectl apply -k config/default` (unchanged). Webhook path: `kubectl apply -k config/overlays/webhook` after CRDs and cert-manager. The overlay composes default + webhook and patches the Deployment. Document exact command sequence in `docs/deploy.md`.

**Reproducible sequence:**
1. Install cert-manager (documented version/command)
2. `kubectl apply -k config/crd`
3. `kubectl apply -k config/overlays/webhook`

## Risks / Trade-offs

| Risk | Mitigation |
|------|------------|
| cert-manager not installed blocks deploy | Document prerequisite; provide install command and version |
| Webhook TLS misconfiguration blocks all create/update | Document rollback: delete ValidatingWebhookConfiguration, MutatingWebhookConfiguration; redeploy minimal |
| First-boot path regresses | Webhook resources in separate overlay; minimal `config/default` unchanged |
| CA bundle injection timing | cert-manager CA injector runs asynchronously; document wait-for-ready if needed |

## Migration Plan

1. Add cert-manager as documented prerequisite
2. Add Service, Certificate, Issuer, ValidatingWebhookConfiguration, MutatingWebhookConfiguration in `config/webhook/`
3. Create overlay `config/overlays/webhook` that patches controller-manager Deployment (--webhook-enabled=true, cert volume)
4. Update cmd/controller-manager main.go (webhook options, --webhook-enabled flag)
5. Update docs/deploy.md with webhook deploy flow and rollback
6. **Rollback:** Delete ValidatingWebhookConfiguration, MutatingWebhookConfiguration; redeploy with `config/default`

## Open Questions

- None. Scope is well-defined.
