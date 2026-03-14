## Why

KubeSwift API types exist but lack admission-time validation and defaulting. Invalid or incomplete specs can reach the controller and cause reconciliation failures or inconsistent behavior. Admission webhooks provide a single point to enforce syntax rules, cross-field constraints, and apply sensible defaults before resources are persisted. This change adds validation and defaulting for SwiftGuest, SwiftImage, and SwiftSeedProfile so that only well-formed resources enter the system, and common fields (architecture, firmware, run policy, etc.) receive consistent defaults without requiring users to specify them explicitly.

## What Changes

- Add admission webhook validation for SwiftGuest, SwiftImage, SwiftSeedProfile
- Add admission webhook defaulting for architecture, firmware, bus, interface model, run policy, shutdown method
- Separate syntax validation (single-resource, no cluster lookups) from cross-resource resolution checks (reference existence, image Ready state)
- Document what is enforced at admission time versus reconcile time
- Place webhook handlers in internal/webhook/ consistent with monorepo layout
- Add ValidatingWebhookConfiguration and MutatingWebhookConfiguration manifests

**Intentionally excluded:**

- Business logic in the webhook beyond validation and defaulting
- Cross-resource resolution at admission time (e.g., fetching SwiftImage to check Ready)
- SwiftGuestClass validation (can be added later)
- Full webhook-server binary implementation (scaffold may exist; this change adds handlers)

## Capabilities

### New Capabilities

- `api-validation-and-defaulting`: Admission webhook validation and defaulting for SwiftGuest, SwiftImage, SwiftSeedProfile; syntax validation separate from cross-resource checks; admission-time vs reconcile-time enforcement boundaries; defaulting rules for architecture, firmware, bus, interface model, run policy, shutdown method; package placement in internal/webhook/.

### Modified Capabilities

- *(none)*

## Impact

- **Repository**: github.com/projectbeskar/kubeswift
- **Paths**: internal/webhook/, config/webhook/, config/crd/ (webhook configs)
- **Binaries**: webhook-server (cmd/webhook-server/) or controller-manager if webhooks run in-process
- **API groups**: swift.kubeswift.io, image.kubeswift.io, seed.kubeswift.io
- **Prerequisites**: add-core-kubeswift-api-types (API types, CRDs)
- **Risks**: Webhook availability affects create/update; failures block admission
- **Rollback**: Remove webhook configs; resources created before rollback remain
