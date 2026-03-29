# API Overview

Five CRDs across four API groups. All `v1alpha1` (no stability guarantee).

## CRDs

| CRD | Short | Scope | Operator use |
|-----|-------|-------|--------------|
| SwiftGuest | `sg` | Namespaced | One VM; references class + image or kernel, optional seed |
| SwiftGuestClass | `sgc` | Cluster | CPU/memory/disk template; reusable |
| SwiftImage | `si` | Namespaced | Disk source (HTTP or PVC clone); must be Ready before SwiftGuest |
| SwiftSeedProfile | `ssp` | Namespaced | NoCloud cloud-init; optional (disk boot only) |
| SwiftKernel | `sk` | Namespaced | Kernel + initramfs OCI artifact; must be Ready before SwiftGuest |

## API groups

| Group | CRDs |
|-------|------|
| `swift.kubeswift.io` | SwiftGuest, SwiftGuestClass |
| `image.kubeswift.io` | SwiftImage |
| `seed.kubeswift.io` | SwiftSeedProfile |
| `kernel.kubeswift.io` | SwiftKernel |

## Typical workflows

### Disk boot (cloud image)

1. Create SwiftGuestClass (e.g. `default`).
2. Create SwiftImage; wait for `phase=Ready`.
3. Create SwiftSeedProfile (optional).
4. Apply RBAC in namespace.
5. Create SwiftGuest with `imageRef`.

### Kernel boot (direct kernel)

1. Label nodes: `kubectl label node <name> kubeswift.io/kernel-node=true`.
2. Create SwiftGuestClass (e.g. `default`).
3. Create SwiftKernel; wait for `phase=Ready`.
4. Apply RBAC in namespace.
5. Create SwiftGuest with `kernelRef`.

[SwiftGuest](swiftguest.md) · [SwiftGuestClass](swiftguestclass.md) · [SwiftImage](swiftimage.md) · [SwiftSeedProfile](swiftseedprofile.md) · [SwiftKernel](swiftkernel.md)
