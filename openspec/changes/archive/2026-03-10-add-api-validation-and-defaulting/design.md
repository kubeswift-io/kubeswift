## Context

KubeSwift API types (SwiftGuest, SwiftGuestClass, SwiftImage, SwiftSeedProfile) exist; CRD schema provides basic type validation. Admission webhooks are the next layer: they enforce cross-field rules, apply defaults, and reject invalid combinations before resources are persisted. This change defines validation and defaulting logic only—no business logic (e.g., no image import triggering, no pod creation).

**Constraints:** internal/webhook/ per monorepo layout; webhook-server or controller-manager hosts handlers; add-core-kubeswift-api-types is prerequisite.

## Goals / Non-Goals

**Goals:**

- Implement admission validation for SwiftGuest, SwiftImage, SwiftSeedProfile
- Implement admission defaulting for architecture, firmware, bus, interface model, run policy, shutdown method
- Separate syntax validation from cross-resource resolution
- Document admission-time vs reconcile-time enforcement
- Place handlers in internal/webhook/ with file layout consistent with repository

**Non-Goals:**

- Business logic in webhook (no reconciliation, no side effects)
- Cross-resource lookups at admission (e.g., fetching SwiftImage to check Ready)
- SwiftGuestClass validation (deferred)

## Decisions

### 1. Syntax validation vs cross-resource resolution

**Syntax validation** (admission webhook, no cluster lookups):

- Validates only the object being created/updated
- No API server calls to fetch referenced resources
- Examples: exactly one boot source, image source exactly one type, format explicit, NoCloud unsupported combinations, memory hotplug maxGuest >= guest memory

**Cross-resource resolution** (reconcile time, controller):

- Requires fetching referenced resources (SwiftImage, SwiftGuestClass, SwiftSeedProfile)
- Examples: imageRef points to existing SwiftImage, SwiftImage is Ready, guestClassRef exists, image architecture matches guest architecture (when both have architecture fields)

**Rationale:** Admission webhooks should be fast and not depend on cluster state. Cross-resource checks belong in the controller, which already performs resolution for reconciliation. If the controller cannot resolve (e.g., image not Ready), it sets a condition and retries.

### 2. Admission time vs reconcile time

| Check | Admission | Reconcile |
|-------|-----------|-----------|
| Exactly one boot source | ✓ | — |
| Image source exactly one type (URL xor PVC) | ✓ | — |
| Image format explicit | ✓ | — |
| NoCloud unsupported combinations | ✓ | — |
| Memory hotplug maxGuest >= guest memory | ✓ | — |
| Required fields present | ✓ (CRD schema) | — |
| imageRef points to existing SwiftImage | — | ✓ |
| SwiftImage is Ready | — | ✓ |
| guestClassRef exists | — | ✓ |
| seedProfileRef exists (if specified) | — | ✓ |
| Image architecture matches guest architecture | — | ✓ (or admission if both in same object) |
| SwiftImage immutable when Ready | ✓ (reject spec mutation) | ✓ (defense in depth) |

**Admission:** Reject or default before persistence. Fast, no lookups.

**Reconcile:** Resolve references, check Ready state, set conditions on failure. Controller retries.

### 3. Package and file layout

```
internal/webhook/
├── suite.go              # Webhook server setup, registration
├── swiftguest/
│   ├── validator.go     # Validate SwiftGuest (syntax only)
│   └── defaulter.go      # Default SwiftGuest fields
├── swiftimage/
│   ├── validator.go      # Validate SwiftImage
│   └── defaulter.go      # Default SwiftImage fields
├── swiftseedprofile/
│   ├── validator.go      # Validate SwiftSeedProfile
│   └── defaulter.go      # Default SwiftSeedProfile fields
└── shared/
    └── validation.go     # Shared validation helpers (optional)
```

**Rationale:** One directory per resource; validator and defaulter separate for clarity. Suite registers all handlers. Consistent with internal/ for non-API code.

### 4. Validation rules (syntax, admission only)

**SwiftGuest:**

- Exactly one boot source (imageRef; no alternate boot sources in MVP)
- If memory hotplug is specified, maxGuest >= guest memory (from GuestClassRef; requires GuestClass in request context or admission skips—see note below)
- runPolicy is Running or Stopped
- imageRef and guestClassRef required; seedProfileRef optional

**Note on memory hotplug:** If SwiftGuest does not embed GuestClass, the webhook cannot resolve guest memory without a lookup. Options: (a) admission skips this check; (b) controller enforces at reconcile; (c) SwiftGuest embeds a copy of relevant GuestClass fields for validation. Design chooses (b) for MVP: controller validates at reconcile. If API adds inline memory/hotplug to SwiftGuest, admission can enforce.

**SwiftImage:**

- Image source must be exactly one type (URL xor PVC; not both, not neither)
- Format must be explicit (raw or qcow2)
- Source URL must be valid format if present; PVC must be non-empty if present

**SwiftSeedProfile:**

- NoCloud cannot include unsupported combinations (e.g., ConfigDrive-specific fields with NoCloud datasource)
- userData required when datasource is NoCloud
- datasource must be NoCloud for MVP (reject ConfigDrive, Ignition, Unattend)

### 5. Defaulting rules

| Field | Resource | Default | Rationale |
|-------|----------|---------|------------|
| architecture | SwiftGuest | x86_64 | Linux-first, Cloud Hypervisor default |
| firmware | SwiftGuest | efi or bios (TBD) | Cloud Hypervisor default |
| bus | SwiftGuest (disk) | virtio | Virtio-first |
| interface model | SwiftGuest (network) | virtio | Virtio-first |
| runPolicy | SwiftGuest | Running | User expectation |
| shutdownMethod | SwiftGuest | ACPI | Graceful shutdown |

**Note:** architecture, firmware, bus, interface model, shutdownMethod may require API type additions if not yet present. This change assumes they exist or will be added; defaulting applies when fields are omitted.

### 6. No business logic in webhook

The webhook MUST NOT:

- Create or update other resources (Pods, ConfigMaps, etc.)
- Trigger image import
- Perform reconciliation
- Call external services
- Mutate status

The webhook MUST only:

- Validate the incoming object (syntax, cross-field rules)
- Apply defaults to the spec
- Return Allow or Deny

### 7. Webhook configuration

- ValidatingWebhookConfiguration for swift.kubeswift.io, image.kubeswift.io, seed.kubeswift.io
- MutatingWebhookConfiguration for same resources
- Failure policy: Fail (reject on webhook failure) or Ignore (allow on failure)—design prefers Fail for validation, configurable
- Object selector: all v1alpha1 resources of SwiftGuest, SwiftImage, SwiftSeedProfile

## Risks / Trade-offs

| Risk | Mitigation |
|------|------------|
| Webhook unavailable blocks all create/update | Failure policy Ignore for availability; or run webhook in controller-manager with high availability |
| API types lack fields for some validations | Add API types in prerequisite change or same change; document dependency |
| Memory hotplug check requires GuestClass | Defer to reconcile; or add optional inline fields to SwiftGuest for admission |
| Defaulting creates hidden behavior | Document all defaults; prefer explicit in API where possible |

## Migration Plan

1. Add internal/webhook/ handlers
2. Register with webhook server (or controller-manager)
3. Add ValidatingWebhookConfiguration, MutatingWebhookConfiguration to config/
4. Deploy; test with sample YAML
5. **Rollback:** Remove webhook configs; admission bypassed; existing resources unchanged

## Open Questions

- Whether architecture, firmware, bus, interface model, shutdownMethod exist in current API types—may require add-core-kubeswift-api-types extension
- Failure policy: Fail vs Ignore for production
