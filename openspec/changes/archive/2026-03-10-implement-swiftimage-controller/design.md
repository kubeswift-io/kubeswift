## Context

SwiftImage API types and CRDs exist (add-core-kubeswift-api-types). The controller must reconcile SwiftImage through a lifecycle from source to Ready, producing a runtime-ready artifact reference. KubeSwift design: explicit format only, runtime-preferred raw, no format guessing, SwiftImage immutable once Ready. This change implements the first controller; format conversion can be stubbed but the design must leave a clean path for real conversion.

**Constraints:** internal/controller/swiftimage/ per monorepo; add-core-kubeswift-api-types prerequisite; image.kubeswift.io.

## Goals / Non-Goals

**Goals:**

- Implement SwiftImage controller reconciling lifecycle through Pending, Importing, Validating, Preparing, Ready, Failed
- Support source types: http, upload (placeholder), pvcClone
- Enforce explicit format; no guessing
- Add prepared storage reference to status
- Enforce immutability once Ready
- Design format conversion as pluggable step (stub allowed)
- Use repository-relative paths

**Non-Goals:**

- Full image upload UX
- Node-local image caching

## Decisions

### 1. Lifecycle phases

| Phase | Meaning |
|-------|---------|
| Pending | Created; not yet started import |
| Importing | Fetching/copying from source (http, pvcClone) or waiting for upload |
| Validating | Verifying image integrity, format |
| Preparing | Converting to runtime format if needed (raw preferred) |
| Ready | Immutable; prepared artifact available |
| Failed | Error; condition carries reason |

**Transitions:** Pending → Importing → Validating → Preparing → Ready. Any phase can transition to Failed. Ready is terminal (no further transitions except deletion).

### 2. Source types

| Source | Behavior |
|--------|----------|
| http | Fetch from URL (http/https). Controller creates Job or Pod to download; stores in PVC. |
| upload | Placeholder. Controller sets phase to Pending or Importing; waits for upload completion (future). For MVP: accept spec, set condition "upload not yet implemented" or similar. |
| pvcClone | Clone from existing PVC. Controller creates Job or uses CSI clone; produces new PVC. |

**API shape:** Extend ImageSource (or equivalent) with:

```go
type ImageSource struct {
    HTTP       *HTTPSource       `json:"http,omitempty"`
    Upload     *UploadSource     `json:"upload,omitempty"`   // placeholder
    PVCClone   *PVCCloneSource  `json:"pvcClone,omitempty"`
}
```

Exactly one source type. Webhook validates (add-api-validation-and-defaulting or this change).

### 3. Explicit format; no guessing

- spec.format MUST be set (raw or qcow2). Webhook rejects empty.
- Controller MUST NOT infer format from URL, file extension, or content. It uses spec.format only.
- Runtime-preferred format is raw. If spec.format is qcow2, the Preparing phase converts to raw (or stubs conversion).

### 4. Prepared storage reference in status

SwiftImageStatus MUST include:

```go
type SwiftImageStatus struct {
    Phase    SwiftImagePhase    `json:"phase"`
    Conditions []Condition      `json:"conditions,omitempty"`
    PreparedArtifact *PreparedArtifactRef `json:"preparedArtifact,omitempty"`
}
type PreparedArtifactRef struct {
    PVCRef   *corev1.TypedLocalObjectReference `json:"pvcRef,omitempty"`   // PVC name, namespace
    Format   DiskFormat                        `json:"format"`             // raw | qcow2
    Size     *resource.Quantity                `json:"size,omitempty"`
}
```

When phase is Ready, preparedArtifact MUST be set. Consumers (SwiftGuest resolver) use this to mount the image.

### 5. Immutability once Ready

- When status.phase is Ready, spec mutations MUST be rejected.
- Enforcement: webhook (add-api-validation-and-defaulting) or controller (reject update, set condition).
- Recommended: webhook rejects spec changes when status.phase is Ready.

### 6. Format conversion as pluggable step

**Design:** The Preparing phase calls a converter interface:

```go
type ImageConverter interface {
    Prepare(ctx context.Context, sourcePath string, sourceFormat, targetFormat DiskFormat) (preparedPath string, size int64, err error)
}
```

- Default implementation: stub—if sourceFormat == targetFormat, return sourcePath; else return error "conversion not implemented".
- Future: real implementation (qemu-img convert, etc.) can be plugged in.
- Runtime-preferred targetFormat is raw. If spec.format is raw, no conversion. If qcow2, conversion to raw in Preparing (or stub).

**Clean path:** Controller passes source path, source format (from spec), target format (raw). Converter is injectable. No format guessing anywhere.

### 7. Package and file layout

```
internal/controller/swiftimage/
├── controller.go    # Reconcile loop, phase transitions
├── import.go        # Import logic (http, pvcClone)
├── validate.go      # Validation step
├── prepare.go       # Preparation step, converter call
├── converter.go     # ImageConverter interface, stub impl
└── status.go        # Status update helpers
```

All paths relative to github.com/projectbeskar/kubeswift.

### 8. Async import via Job

For http and pvcClone, import can be long-running. Controller creates a Job (or Pod) to perform the work. Controller watches Job status; when Job completes, transitions to Validating. Job template in internal/controller/swiftimage/ or config/.

### 9. Conditions

Standard conditions: Ready (True when phase=Ready), and failure reason when Failed. Additional conditions for Importing (e.g., JobRunning), Validating, Preparing as needed for observability.

## Risks / Trade-offs

| Risk | Mitigation |
|------|------------|
| Long-running import blocks reconcile | Use Job; controller watches Job status; requeue on Job completion |
| Format conversion not implemented | Stub returns error for qcow2→raw; document; add real impl later |
| Upload placeholder incomplete | Clear condition; document in status |
| PVC clone requires CSI | Document prerequisite; fail with clear message if not supported |

## Migration Plan

1. Add SwiftImage controller
2. Register with controller-manager
3. Extend SwiftImage status if needed (preparedArtifact)
4. Extend ImageSource if needed (http, upload, pvcClone)
5. Deploy; test with http source
6. **Rollback:** Disable controller; remove from manager; existing Ready images remain

## Open Questions

- Whether to use Job or Pod for import (Job is standard for one-off work)
- Exact PVC naming and storage class for prepared artifacts
