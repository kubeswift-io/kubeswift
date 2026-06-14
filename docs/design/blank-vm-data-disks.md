# Blank / raw VM data disks (data disks without an image)

> Status: DESIGN (staff-architect, 2026-06-14). Target: v0.4.2.
> Implementation: one PR on the v0.4.2 branch, Block-blank first.

## Problem

KubeSwift's only VM-visible secondary disk is **image-backed**: `dataDiskRef`
clones a SwiftImage into `/var/lib/kubeswift/disks/data/image.raw`, which Cloud
Hypervisor attaches as a `--disk` (appears as `/dev/vdc` on a disk-boot guest).
There is **no blank / size-only VM data disk**. The #1 data-disk use case — "give
me a blank 100 GiB volume for my database" — is impossible: operators must host a
blank raw image, and pointing `dataDiskRef` at a bootable OS image **silently
corrupts the boot** (the OS image's partition/filesystem UUIDs collide with the
root disk → the guest mounts a mix of partitions from both disks; observed in the
data-disk demo, warning added in `docs/api/swiftguest.md`).

## Current state (verified)

| Path | VM disk? | Mechanism |
|---|---|---|
| `spec.dataDiskRef` (singular → SwiftImage) | **YES** | `resolver.go::resolveDataDisk` → `rg.DataDisk` → `runtimeintent/build.go` `DataDisk={Path: DisksDataPath+"/image.raw"}` → CH `--disk`. |
| `spec.dataDiskRefs[].pvcRef` | **NO** | `pod.go::applyDataDiskRefs` mounts it as a **filesystem dir** (`/disks/pvc-<name>`) — the SwiftGuestPool per-replica-storage path. |
| `spec.dataDiskRefs[].imageRef` | **NO — dead code** | The resolver only ever reads the **singular** `dataDiskRef`; nothing reads `dataDiskRefs[].imageRef`. The webhook does not validate `dataDiskRefs` at all. |

The **W9 Block-mode root disk** already attaches a Block PVC as a raw VM disk
(`DiskRootDevicePath`, `volumeDevices`, opaque CH `--disk path=/dev/...`). The
Rust side treats `--disk path=` opaquely (no suffix logic) and is **single-data-
disk only** today (`data_disk_path: String`).

## Design

### API — extend the plural `DataDiskRef` struct
A blank disk is a third kind of data disk, alongside the (now-real) image-backed
and attached-PVC kinds, in the plural `dataDiskRefs[]`:

```go
type DataDiskRef struct {
    Name         string                       // DNS-label, unique, <=36
    ImageRef     *corev1.LocalObjectReference // image-backed (one of three)
    PVCRef       *corev1.LocalObjectReference // attach an existing PVC
    Blank        *BlankDiskSpec               // NEW: blank, sized, no image
    AttachAsDisk bool                         // for pvcRef: raw VM disk vs fs-mount
}
type BlankDiskSpec struct {
    Size             resource.Quantity            // required, > 0
    StorageClassName *string                      // empty = default class
    VolumeMode       corev1.PersistentVolumeMode  // Block (default) | Filesystem
}
```
Exactly one of `imageRef`/`pvcRef`/`blank` per entry. The legacy singular
`dataDiskRef` stays as a one-image shorthand. `attachAsDisk` (default false)
keeps the SwiftGuestPool `pvcRef` filesystem-mount behaviour untouched.

### Runtime — Block-mode PVC → raw `--disk path=/dev/...` (reuse W9)
Default `blank.volumeMode: Block`: the controller creates a guest-owned,
sized, **Block** PVC (nothing is copied — there's no image), attached via
`volumeDevices` at `/dev/kubeswift-data-<name>` and passed to CH opaquely. The
guest partitions and formats it (`mkfs`). `Filesystem` is an escape hatch for
Block-incapable clusters (a blank `image.raw` is created). Multiple blank disks
get distinct device paths; they enumerate in the guest as `/dev/vdc, /dev/vdd,
...` in declaration order — operators should **mount by UUID/label**, not by
`/dev/vdX`.

### The load-bearing refactor: singular → slice
- `resolved.ResolvedGuest.DataDisk *PreparedImage` → `DataDisks []ResolvedDataDisk`.
- `runtimeintent.RuntimeIntent.DataDisk *RootDiskSpec` → `DataDisks []DataDiskSpec`.
- Rust: **dual-field deserialize** — keep `data_disk: Option<RootDisk>` AND add
  `data_disks: Option<Vec<DataDisk>>` (both `#[serde(default)]`); prefer the
  array when present. Rolling-upgrade safe both directions (the DRA-arc
  null-vs-absent lesson: `omitempty` + `serde(default)`, never emit
  `"dataDisks": null`). Emit one `--disk` per entry.
- Do this now: blank-only-on-the-singular would force the exact same refactor
  again later. Blast radius ~10 call sites (`pod.go`, `gpu.go`, `build.go`,
  `resolver.go`), all mechanical; add a `dataDiskMount(disk)` helper (mirrors the
  W9 `rootDiskMount`).

### Controller
New `EnsureBlankDataDisks(ctx, guest, rg)` (a `datadisk.go` mirroring
`rootdisk.go::EnsureRootDiskClone`), called right after `EnsureRootDiskClone`.
For each blank disk: create a guest-owned (owner-ref'd → GC) PVC, RWO,
`VolumeMode` from spec, `storage=Size`, `StorageClassName` from spec. **No Job**
(nothing to copy). Gate the pod on all blank PVCs `Bound`.

### Validation (webhook `validateDataDisks`, per-operation discipline)
Exactly one of imageRef/pvcRef/blank per entry; `blank.size > 0`; `volumeMode`
enum; name required/DNS-label/**unique**; `attachAsDisk` only with `pvcRef`; max
**8** data disks. (Closes the pre-existing gap: `dataDiskRefs` is unvalidated
today.) Data disks compose with GPU — **no** `usesGPU` rejection.

### No silent failures
- `status.dataDisks []DataDiskStatus` (`{Name, PVCName, VolumeMode, DevicePath,
  Bound}`) echoed by the controller (mirrors `status.storage`).
- A `DataDisksReady=False` condition (reason `BlankPVCPending` + PVC name/phase)
  when a blank PVC can't bind (e.g. a Filesystem-only class for a Block disk);
  the guest stays Pending — never boots with a missing disk silently.
- Wiring `dataDiskRefs[].imageRef` retires today's silent-ignore of that field.

## Phasing — one PR, suggested commit order
1. Resolver/types slice refactor + `RuntimeIntent.DataDisks` (behaviour-
   preserving for the singular path; Go tests stay green).
2. Rust dual-field deserialize + per-disk `--disk` loop (`swift-ch-client`,
   `swift-qemu-client`, `swiftletd`).
3. CRD `BlankDiskSpec`/`AttachAsDisk` + webhook `validateDataDisks` +
   `make generate` + chart sync.
4. Controller `EnsureBlankDataDisks` + pod-builder Block `volumeDevices` loop +
   `status.dataDisks` + `DataDisksReady`.
5. Sample (`config/samples/datadisk/`) + runbook + an updated field-testing demo
   12 (blank Block disk instead of a colliding OS image).

The singular→slice refactor must be **atomic** (commit 1) — a half-migrated
`rg.DataDisk`/`RuntimeIntent.DataDisk` is worse than either end state.

## Risks
- **Device-letter determinism** (HIGH): `/dev/vdc` vs `/dev/vdd` is emission-order
  derived. One ordering source of truth (singular first, then `dataDiskRefs[]`
  index order) shared by the pod builder and the intent array; document mount-by-
  UUID. Host device path `/dev/kubeswift-data-<name>` is stable; the guest letter
  is not.
- **Resolver singular→slice blast radius** (MEDIUM): ~10 mechanical sites; the
  singular path has existing test coverage.
- **Plural `dataDiskRefs` fs-vs-VM-disk split** (MEDIUM): the same field now
  yields a filesystem dir (`pvcRef` alone) or a VM disk (`imageRef`/`blank`/
  `pvcRef`+`attachAsDisk`); narrow `applyDataDiskRefs` to exactly the fs case and
  assert the split in tests. SwiftGuestPool (`pvcRef`-only) unaffected.
- **RuntimeIntent JSON contract** (`dataDisk`→`dataDisks`, MEDIUM): handled by the
  dual-field Rust deserialize; add a Go↔Rust round-trip test.
- **Block PVC on Block-incapable clusters** (LOW): opt-in; surfaced via
  `DataDisksReady`, never silent; cluster `longhorn` class is Block-capable.
- **Migration interaction** (LOW, document): guest-owned RWO blank PVCs
  detach/reattach like the root PVC on offline migration (already handled by the
  Preparing dual-poll); live-migration of data disks is a separate later design.
