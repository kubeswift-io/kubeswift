## Why

Linux cloud guests need cloud-init-compatible seed data to bootstrap. KubeSwift must deliver NoCloud datasource media (user-data, meta-data, network-config) without reimplementing cloud-init. The control plane resolves these from SwiftGuest and optional SwiftSeedProfile, creates a Kubernetes artifact (ConfigMap) with text content, and the node runtime (swiftletd or Rust helper) consumes that artifact to build local seed media. This change implements the control-plane seed rendering: resolution of user-data, meta-data, network-data from references (including Secret and ConfigMap), creation of a text-based ConfigMap, and a clear contract for the node runtime to consume it—keeping KubeSwift as a datasource delivery layer, not a cloud-init reimplementation.

## What Changes

- Resolve user-data, meta-data, and network-data from SwiftGuest and optional SwiftSeedProfile
- Support Secret and ConfigMap references for userData, metaData, networkData where designed
- Create a ConfigMap (or equivalent) with NoCloud-standard keys (user-data, meta-data, network-config) as text
- Mount the ConfigMap into the pod envelope for node runtime consumption
- Keep generated artifact text-based (no ISO blobs in API server)
- Add Rust helper (rust/swift-seed) or swiftletd logic to build local NoCloud media from ConfigMap
- Document contract between control plane and node runtime

**Intentionally excluded:**

- ConfigDrive
- Ignition
- Windows unattended setup

## Capabilities

### New Capabilities

- `seed-rendering-nocloud`: Control-plane seed rendering for NoCloud; resolve user-data, meta-data, network-data from SwiftGuest and SwiftSeedProfile; support Secret and ConfigMap refs; create text-based ConfigMap artifact; node runtime consumes artifact to build local seed media; KubeSwift does not reimplement cloud-init; local seed media generation in swiftletd or Rust helper.

### Modified Capabilities

- *(none)*

## Impact

- **Repository**: github.com/projectbeskar/kubeswift
- **Paths**: internal/controller/swiftguest/ (or internal/seed/), rust/swift-seed/, api/seed/v1alpha1/ (status or ref extensions if needed)
- **Prerequisites**: add-core-kubeswift-api-types, add-swiftguest-resolver

- **Dependencies**: ResolvedGuest.Seed from resolver
- **Risks**: Secret/ConfigMap ref resolution adds complexity; node runtime must consume artifact correctly
- **Rollback**: Remove seed rendering; guests without seed profile continue to work
