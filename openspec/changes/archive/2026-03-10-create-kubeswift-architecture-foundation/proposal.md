## Why

KubeSwift needs a documented architecture foundation before implementation begins. A new Kubernetes-native virtualization platform built around Cloud Hypervisor—rather than extending KubeVirt—is justified because KubeVirt is libvirt/QEMU-centric, carries legacy abstractions, and targets broad hypervisor compatibility. KubeSwift intentionally avoids that path: it targets modern cloud-style VMs only, uses Cloud Hypervisor as the sole VMM, and keeps VM semantics explicit. This change records the architectural decisions, language split, API groups, primary resources, and runtime model so that all subsequent work aligns with a single, coherent direction.

## What Changes

- Establish the initial architecture direction for KubeSwift as a Cloud-Hypervisor-native virtualization platform.
- Record the language split: Go 1.25 for control plane, Rust for node runtime and low-level VM helpers.
- Record primary API groups: `swift.kubeswift.io`, `image.kubeswift.io`, `seed.kubeswift.io`.
- Record primary initial resources: `SwiftGuest`, `SwiftGuestClass`, `SwiftImage`, `SwiftSeedProfile`.
- Record the core runtime model: one guest uses one pod envelope; `swiftletd` launches and manages Cloud Hypervisor on the node.
- Record that KubeSwift supports cloud-init compatibility through generated datasource media, not by reimplementing cloud-init.
- Record that image import/preparation and guest initialization are separate subsystems.
- Create architecture documentation, API scaffolding, and initial repository structure in github.com/projectbeskar/kubeswift.

**Intentionally excluded:**

- Full implementation of controllers
- Full implementation of swiftletd
- Live migration
- Snapshots
- Windows guest support
- KubeVirt compatibility layers
- Multi-hypervisor support

## Capabilities

### New Capabilities

- `architecture-foundation`: Architecture-level requirements and constraints for KubeSwift, including language split, API groups, primary resources, runtime model (one guest per pod, swiftletd), cloud-init approach (datasource media only), and separation of image vs. initialization subsystems.

### Modified Capabilities

- *(none)*

## Impact

- **Repository**: github.com/projectbeskar/kubeswift (monorepo)
- **API groups**: `swift.kubeswift.io`, `image.kubeswift.io`, `seed.kubeswift.io` (v1alpha1)
- **Binaries**: Control plane manager (Go), `swiftletd` (Rust, scaffold only)
- **Packages**: Go API types, controller-runtime setup, Rust crate layout
- **Docs**: `docs/architecture/` for architecture documentation
- **Risks**: New project with no prior implementation; decisions are foundational and should be stable.
- **Dependencies**: Go 1.25, Rust toolchain, controller-runtime, Kubebuilder
- **Rollback**: Remove scaffolding; no production systems affected.
