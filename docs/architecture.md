# KubeSwift Architecture

KubeSwift is **Cloud-Hypervisor-native**: it uses Cloud Hypervisor as the sole VMM. There is no libvirt, no QEMU, no multi-hypervisor abstraction. The design is intentionally narrow.

## Why Cloud Hypervisor only?

- **Simplicity** — One hypervisor, one integration path. No abstraction layer to maintain.
- **Cloud focus** — Cloud Hypervisor targets modern Linux cloud images and virtio devices.
- **Explicit VM semantics** — Start, stop, restart are first-class; no VM-as-pod indirection.

## Design principles

| Principle | Meaning |
|-----------|---------|
| **Cloud Hypervisor only** | No libvirt/QEMU; direct Unix-socket integration |
| **One guest per pod** | Each SwiftGuest becomes one pod; swiftletd runs as the launcher container |
| **Kubernetes-native** | Scheduling, networking, storage via standard Kubernetes primitives |
| **Linux cloud images** | Targets raw/qcow2 images with cloud-init (NoCloud) |

## Components

| Component | Where it runs | Purpose |
|-----------|---------------|---------|
| **controller-manager** | Cluster (Deployment) | SwiftImage + SwiftGuest controllers; optional admission webhooks |
| **swiftletd** | Inside each SwiftGuest pod (launcher container) | Reads runtime intent, builds NoCloud seed, launches Cloud Hypervisor, reports status |
| **swiftctl** | CLI | Operator tooling |
| **Helm chart** | OCI registry | `oci://ghcr.io/projectbeskar/charts/kubeswift` |

## End-to-end data flow

1. **Operator** creates SwiftGuest (references SwiftGuestClass, SwiftImage, optional SwiftSeedProfile).
2. **SwiftGuest controller** resolves refs, renders NoCloud seed, creates runtime-intent ConfigMap.
3. **Controller** creates a Pod with: root-disk PVC, seed ConfigMap volume (if present), runtime-intent ConfigMap.
4. **swiftletd** (launcher) reads intent, builds NoCloud media, spawns Cloud Hypervisor.
5. **Cloud Hypervisor** runs the VM; swiftletd patches SwiftGuest status (`GuestRunning`).

See [control plane](architecture/control-plane.md), [node runtime](architecture/node-runtime.md), [lifecycle](architecture/lifecycle.md).
