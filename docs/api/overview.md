# API Overview

Four CRDs across three API groups. All `v1alpha1` (no stability guarantee).

## CRDs

| CRD | Short | Scope | Operator use |
|-----|-------|-------|--------------|
| SwiftGuest | `sg` | Namespaced | One VM; references class, image, optional seed |
| SwiftGuestClass | `sgc` | Cluster | CPU/memory/disk template; reusable |
| SwiftImage | `si` | Namespaced | Disk source (HTTP or PVC clone); must be Ready before SwiftGuest |
| SwiftSeedProfile | `ssp` | Namespaced | NoCloud cloud-init; optional |

## API groups

| Group | CRDs |
|-------|------|
| `swift.kubeswift.io` | SwiftGuest, SwiftGuestClass |
| `image.kubeswift.io` | SwiftImage |
| `seed.kubeswift.io` | SwiftSeedProfile |

## Typical workflow

1. Create SwiftGuestClass (e.g. `default`).
2. Create SwiftImage; wait for `phase=Ready`.
3. Create SwiftSeedProfile (optional).
4. Apply RBAC in namespace.
5. Create SwiftGuest.

[SwiftGuest](swiftguest.md) · [SwiftGuestClass](swiftguestclass.md) · [SwiftImage](swiftimage.md) · [SwiftSeedProfile](swiftseedprofile.md)
