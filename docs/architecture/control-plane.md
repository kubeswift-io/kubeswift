# Control Plane

The control plane runs in the **controller-manager** Deployment (`kubeswift-system`). It reconciles SwiftImage and SwiftGuest and optionally serves admission webhooks.

## Controllers

### SwiftImage controller

| Action | Behavior |
|--------|----------|
| HTTP source | Creates Import Job to download image into PVC |
| PVC clone source | Clones or references existing PVC |
| Status | Sets `phase` (Importing → Ready/Failed), `preparedArtifact.pvcRef` |

[SwiftImage API](../api/swiftimage.md)

### SwiftGuest controller

| Step | Behavior |
|------|----------|
| Resolve | Fetches SwiftGuestClass, SwiftImage, SwiftSeedProfile; fails if refs missing or SwiftImage not Ready |
| Seed | Renders NoCloud user-data/meta-data/network-config from SwiftSeedProfile into ConfigMap `<guest>-seed` |
| Intent | Builds runtime-intent JSON (CPU, memory, disk path, seed path) into ConfigMap `<guest>-runtime-intent` |
| Pod | Creates pod with root-disk PVC, seed volume (optional), intent volume; launcher = swiftletd |
| Status | Maps pod phase → SwiftGuest phase; swiftletd reports `GuestRunning` |

[SwiftGuest API](../api/swiftguest.md) · [Reconcile flow](../swiftguest-reconcile.md)

## Admission webhooks (optional)

With `--webhook-enabled=true` and cert-manager:

- **Validating** — Required refs, runPolicy enum
- **Mutating** — Defaults

Without webhooks, create/update succeeds; validation happens at reconcile time.

## Deployment

- **Namespace:** `kubeswift-system`
- **Manifests:** `config/manager/`, `config/default/`
- **Webhook overlay:** `config/overlays/webhook/` (requires cert-manager)

[Install](../install/)
