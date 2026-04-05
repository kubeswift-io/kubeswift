# API Overview

Eight CRDs across five API groups. All `v1alpha1` (no stability guarantee).

## CRDs

| CRD | Short | Scope | Operator use |
|-----|-------|-------|--------------|
| SwiftGuest | `sg` | Namespaced | One VM; references class + image or kernel, optional seed and GPU profile |
| SwiftGuestPool | `sgpool` | Namespaced | Fleet of identical VMs; replicas, rolling updates, spread, PVCs |
| SwiftGuestClass | `sgc` | Cluster | CPU/memory/disk template; reusable |
| SwiftImage | `si` | Namespaced | Disk source (HTTP or PVC clone); must be Ready before SwiftGuest |
| SwiftSeedProfile | `ssp` | Namespaced | NoCloud cloud-init; optional (disk boot only) |
| SwiftKernel | `sk` | Namespaced | Kernel + initramfs OCI artifact; must be Ready before SwiftGuest |
| SwiftGPUProfile | `sgp` | Namespaced | GPU passthrough request (count, model, tier, topology) |
| SwiftGPUNode | `sgn` | Cluster | Per-node GPU inventory; auto-populated by Discovery DaemonSet |

## API groups

| Group | CRDs |
|-------|------|
| `swift.kubeswift.io` | SwiftGuest, SwiftGuestPool, SwiftGuestClass |
| `image.kubeswift.io` | SwiftImage |
| `seed.kubeswift.io` | SwiftSeedProfile |
| `kernel.kubeswift.io` | SwiftKernel |
| `gpu.kubeswift.io` | SwiftGPUProfile, SwiftGPUNode |

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

### GPU boot (disk boot + GPU passthrough)

1. Label GPU nodes: `kubectl label node <name> kubeswift.io/gpu-node=true`.
2. Deploy GPU Discovery DaemonSet; wait for SwiftGPUNode to show `phase=Ready`.
3. Create SwiftGPUProfile describing GPU requirements.
4. Create SwiftGuestClass, SwiftImage (Ready), SwiftSeedProfile.
5. Create SwiftGuest with `imageRef` + `gpuProfileRef`.
6. SwiftGPU controller allocates GPUs; SwiftGuest controller creates pod with VFIO devices.

### Fleet management

1. Create prerequisite resources (SwiftGuestClass, SwiftImage, optional SwiftSeedProfile and SwiftGPUProfile).
2. Create SwiftGuestPool with `replicas` and a `template` containing the SwiftGuest spec.
3. Controller creates replicas named `<pool>-0` through `<pool>-<N-1>`.
4. Scale with `kubectl scale sgpool <name> --replicas=N`.
5. Update `template.spec` to trigger a rolling update.

[SwiftGuest](swiftguest.md) · [SwiftGuestPool](swiftguestpool.md) · [SwiftGuestClass](swiftguestclass.md) · [SwiftImage](swiftimage.md) · [SwiftSeedProfile](swiftseedprofile.md) · [SwiftKernel](swiftkernel.md) · [SwiftGPUProfile](swiftgpuprofile.md) · [SwiftGPUNode](swiftgpunode.md)
