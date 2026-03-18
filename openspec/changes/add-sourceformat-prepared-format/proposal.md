## Why

Operators need visibility into the disk format pipeline for SwiftImage: the original source format (e.g. qcow2 from cloud images) and the prepared runtime format (always raw for Cloud Hypervisor). Today this information is implicit—spec.format exists but status does not record what was actually imported or prepared. Adding sourceFormat and preparedFormat to SwiftImageStatus provides observability for debugging and auditing without changing behavior.

## What Changes

- Add `SourceFormat` and `PreparedFormat` fields (type `DiskFormat`) to `SwiftImageStatus` in `api/image/v1alpha1/swiftimage_types.go`
- Regenerate CRDs via `make generate` and apply to cluster

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `core-api-types`: SwiftImageStatus SHALL include sourceFormat and preparedFormat (DiskFormat) for format observability.

## Impact

- **Code**: `api/image/v1alpha1/swiftimage_types.go` (SwiftImageStatus struct)
- **CRD**: Regenerated schema in `config/crd/bases/`
- **Controllers**: May populate these fields in a follow-up; this change only adds the API fields
