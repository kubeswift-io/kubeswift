## Context

KubeSwift needs Go API types and CRD scaffolding for SwiftGuest, SwiftGuestClass, SwiftImage, and SwiftSeedProfile. The monorepo layout (api/, config/) exists; this change populates the API packages with structs, status subresources, validation, and CRD manifests. MVP scope: one root disk, one network, NoCloud seed, Linux-first.

**Constraints:** api/ layout per bootstrap; swift.kubeswift.io, image.kubeswift.io, seed.kubeswift.io; no KubeVirt naming; formats explicitly declared.

## Goals / Non-Goals

**Goals:**

- Define Go types for SwiftGuest, SwiftGuestClass, SwiftImage, SwiftSeedProfile
- Add status subresources where controller-reported state is required
- Establish package layout, versioning, validation boundaries, default assignment
- Generate CRDs from Go types
- Add sample YAML

**Non-Goals:**

- Full controller or webhook implementation
- Resolver implementation
- Multiple disks/networks, ConfigDrive, Ignition, Windows

## Decisions

### 1. Package layout under api/

```
api/
├── swift/v1alpha1/
│   ├── doc.go
│   ├── swiftguest_types.go
│   ├── swiftguestclass_types.go
│   ├── groupversion_info.go
│   └── zz_generated.deepcopy.go
├── image/v1alpha1/
│   ├── doc.go
│   ├── swiftimage_types.go
│   ├── groupversion_info.go
│   └── zz_generated.deepcopy.go
├── seed/v1alpha1/
│   ├── doc.go
│   ├── swiftseedprofile_types.go
│   ├── groupversion_info.go
│   └── zz_generated.deepcopy.go
└── shared/
    └── common_types.go    # LocalObjectReference, conditions, etc.
```

**Rationale:** One package per API group+version; shared types in api/shared/ to avoid circular imports. SwiftGuest and SwiftGuestClass share swift.kubeswift.io, so both live in api/swift/v1alpha1/.

### 2. Versioning: v1alpha1 only

**Rationale:** First API version; no v1beta1 or v1 yet. All resources use v1alpha1. When we stabilize, we add new versions and conversion; for now, single version keeps scope small.

### 3. Validation boundaries

| Layer | Responsibility | Where |
|-------|----------------|-------|
| **CRD schema** | Required fields, types, enums, format (e.g., quantity) | OpenAPI in CRD |
| **Webhook** | Cross-field validation, reference existence (e.g., imageRef points to existing SwiftImage), immutable spec checks | internal/webhook/ |
| **Controller** | Runtime validation (e.g., image not Ready), conflict resolution | internal/controller/ |

**MVP:** CRD schema only. Webhook and controller validation deferred to later changes. CRD MUST reject invalid types (e.g., negative cpu).

### 4. Default assignment: webhook vs resolver vs controller

| Default | Where | Rationale |
|---------|-------|-----------|
| `runPolicy: Running` | **Webhook** | User-facing default; applied at create/update so stored spec is always explicit |
| `format: raw` | **Webhook** | User-facing; SwiftImage format preferred |
| `datasource: NoCloud` | **Webhook** | User-facing; SwiftSeedProfile MVP |
| `rootDisk.format` | **Webhook** | User-facing; SwiftGuestClass |
| Resolved CPU/memory from GuestClass | **Resolver** | Controller merges GuestClass into resolved spec; not stored in API |
| Pod resource requests | **Controller** | Derived from resolved spec; not stored in API |
| Status conditions | **Controller** | Runtime state; never defaulted |

**Principle:** Webhook defaults only for fields that belong in the stored API spec. Resolver/controller defaults for derived or runtime state.

### 5. SwiftGuest spec shape

```go
type SwiftGuestSpec struct {
    ImageRef       LocalObjectReference `json:"imageRef"`       // required
    GuestClassRef  LocalObjectReference `json:"guestClassRef"` // required
    SeedProfileRef *LocalObjectReference `json:"seedProfileRef,omitempty"` // optional
    RunPolicy      RunPolicy            `json:"runPolicy,omitempty"`       // default: Running
}
type RunPolicy string
const RunPolicyRunning RunPolicy = "Running"
const RunPolicyStopped  RunPolicy = "Stopped"
```

One root disk, one network: implied by GuestClassRef (GuestClass defines root disk; network comes from pod). No explicit disk/network arrays in spec.

### 6. SwiftGuestClass spec shape

```go
type SwiftGuestClassSpec struct {
    CPU      resource.Quantity `json:"cpu"`
    Memory   resource.Quantity `json:"memory"`
    RootDisk RootDiskSpec      `json:"rootDisk"`
}
type RootDiskSpec struct {
    Size   resource.Quantity `json:"size"`
    Format DiskFormat        `json:"format"` // raw | qcow2
}
```

### 7. SwiftImage spec shape

```go
type SwiftImageSpec struct {
    Source ImageSource `json:"source"` // URL or PVC
    Format DiskFormat  `json:"format"` // raw | qcow2, explicit
}
type ImageSource struct {
    URL *string `json:"url,omitempty"`
    PVC *string `json:"pvc,omitempty"` // "name" or "namespace/name"
}
```

Size optional (for import; can be inferred). Format MUST be explicit.

### 8. SwiftSeedProfile spec shape

```go
type SwiftSeedProfileSpec struct {
    Datasource DatasourceType `json:"datasource"` // NoCloud for MVP
    UserData   string         `json:"userData"`   // cloud-init user-data
    MetaData   string         `json:"metaData,omitempty"`
}
```

MVP: NoCloud only. UserData as inline string; SecretRef can be added later.

### 9. Status subresources

| Resource | Status subresource | Reason |
|----------|-------------------|--------|
| SwiftGuest | Yes | Phase, conditions, guest IP (from controller) |
| SwiftImage | Yes | Phase, conditions (Pending/Importing/Ready/Failed) |
| SwiftGuestClass | No | Cluster-scoped template; no status |
| SwiftSeedProfile | No | Cluster-scoped template; no status |

CRD `subresources: status: {}` for SwiftGuest and SwiftImage.

### 10. CRD generation

Use controller-gen (from controller-tools) with `generate` and `manifests` targets. Output to config/crd/bases/. Kustomize patch for `spec.preserveUnknownFields: false` if needed.

## Risks / Trade-offs

| Risk | Mitigation |
|------|------------|
| API shape too early | Keep MVP minimal; add fields later |
| Webhook defaults not implemented | Document in design; defer to webhook change |
| Shared types in api/shared/ | Keep minimal; avoid pulling in controller-runtime |
| CRD schema validation gaps | Add validation markers; controller-gen generates CEL or OpenAPI |

## Migration Plan

1. Add api packages and types
2. Add controller-gen, generate manifests
3. Add sample YAML
4. **Rollback:** Remove api/ types and config/crd/; no runtime impact

## Open Questions

- Whether SwiftGuestClass should have status (e.g., "Ready" when valid) — deferred for MVP
- Whether UserData should support SecretRef from the start — inline string for MVP
