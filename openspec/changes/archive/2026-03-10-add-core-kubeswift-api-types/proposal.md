## Why

KubeSwift needs concrete API types and CRD scaffolding before controllers can reconcile resources. The architecture and monorepo layout are established, but SwiftGuest, SwiftGuestClass, SwiftImage, and SwiftSeedProfile exist only as names. This change defines the Go structs, status subresources, validation boundaries, and CRD manifests so that the control plane can register and operate on these resources. It keeps the MVP scope small: one root disk, one network, NoCloud seed, Linux-first assumptions—aligned with the Cloud Hypervisor–native design.

## What Changes

- Define Go API types for SwiftGuest, SwiftGuestClass, SwiftImage, SwiftSeedProfile in api/swift/v1alpha1/, api/image/v1alpha1/, api/seed/v1alpha1/
- Add status subresources for resources that require controller-reported state (SwiftGuest, SwiftImage)
- Add CRD manifests with OpenAPI validation schema
- Add CRD generation (controller-gen or equivalent) from Go types
- Add sample YAML for each resource
- Establish package layout, versioning (v1alpha1), validation boundaries, and default assignment (webhook vs resolver vs controller)

**Intentionally excluded:**

- Full controller implementation
- Webhook implementation (validation/mutation)
- Resolver implementation
- Multiple disks or networks
- ConfigDrive, Ignition, Unattend
- Windows guest support
- KubeVirt naming or compatibility

## Capabilities

### New Capabilities

- `core-api-types`: Go API types and CRD scaffolding for SwiftGuest, SwiftGuestClass, SwiftImage, SwiftSeedProfile; package layout under api/; status subresources; validation boundaries; default assignment (webhook vs resolver vs controller); CRD generation and sample YAML.

### Modified Capabilities

- *(none)*

## Impact

- **Repository**: github.com/projectbeskar/kubeswift
- **Paths**: api/swift/v1alpha1/, api/image/v1alpha1/, api/seed/v1alpha1/, config/crd/, config/samples/
- **API groups**: swift.kubeswift.io, image.kubeswift.io, seed.kubeswift.io (v1alpha1)
- **Dependencies**: controller-gen (or Kubebuilder), k8s.io/api, client-go
- **Risks**: API shape decisions affect future evolution; keeping MVP scope small limits lock-in
- **Rollback**: Remove API types and CRDs; no runtime impact
