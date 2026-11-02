# SwiftImage Clone Strategies

> Audience: KubeSwift operators choosing between `cloneStrategy: copy` and `cloneStrategy: snapshot` for SwiftImages.

## Why this exists

When a SwiftGuest boots from a SwiftImage, KubeSwift produces a per-guest root-disk PVC seeded from the image. Two strategies are supported:

| Strategy | How clones are produced | Drivers it works on |
|---|---|---|
| `copy` (default) | Per-guest Copy Job: `cp` from the SwiftImage PVC to the new PVC, then `qemu-img resize` + `sgdisk -e` | Any CSI driver, including non-snapshot-capable ones (local-path, NFS, hostPath) |
| `snapshot` | CSI VolumeSnapshot of the SwiftImage PVC + per-guest `dataSource: VolumeSnapshot` clones, expand-and-wait gate before guest pod schedule | Snapshot-capable CSI drivers (Longhorn, Rook Ceph, EBS, GCE PD) |

On a true copy-on-write driver (Rook Ceph RBD, EBS, GCE PD) `snapshot` skips the per-guest `cp` (≈8–12 GiB read + write for Ubuntu Noble): the `dataSource` clone and the expand are metadata operations, not data copies. On a full-copy driver (Longhorn) it does not save the copy, and is usually slower.

Measured on a three-node test cluster (v0.22.0, Ubuntu Noble, image disk the same size as the guest's, so no resize; seconds from creating the SwiftGuest to the guest's IP):

| Storage | Strategy | 1 guest | 4 guests at once (first / last) |
|---|---|---|---|
| Longhorn | `copy` | 79 | 176 / 247 |
| Longhorn | `snapshot` | 160 | 163 / 287 |
| Rook Ceph RBD (Filesystem) | `copy` | 132 | not measured |
| Rook Ceph RBD (Filesystem) | `snapshot` | 72 | 90 / 92 |

> **Longhorn: keep `copy`.** A Longhorn `dataSource` clone is itself a full copy,
> made by the storage instead of a Job, and it was slower than `copy` both for one
> guest and for four at once, even with no resize. The earlier ≈3× Longhorn
> figure (Phase 0 spike, small source) did not reproduce. With a large resize
> delta it is slower still: the PVC is created at the *source* size and expanded
> (Longhorn refuses different-size clones), then `clone-grow-init` runs (see the
> F7 finding in [`../snapshots/operator-walkthrough.md`](../snapshots/operator-walkthrough.md)).
> While a Longhorn clone is still copying, the guest already reports
> `StorageReady=True`; it starts once the clone finishes (#909).
>
> **Rook Ceph RBD: `snapshot` is ≈2× faster for one guest and ≈2.7× for four**,
> and the four finish together because no data is copied. Filesystem root disks
> only (see the warning below).

The default is **`copy`** for backward compatibility. Existing SwiftImages keep working unchanged.

## Choosing a strategy

Use **copy** when:
- Your storage class is local-path / NFS / hostPath — anything without VolumeSnapshot support.
- Your storage is a full-copy driver such as Longhorn (`snapshot` is slower there).
- The root disk is `volumeMode: Block` on Rook Ceph RBD (see the warning below).
- You only run a handful of guests per image and start-up speed isn't a hot path.
- You want behaviour identical to KubeSwift v0.1.

Use **snapshot** when:
- Your storage class is backed by a copy-on-write CSI driver (Rook Ceph RBD with a Filesystem root disk, EBS, GCE PD).
- You scale a SwiftGuestPool, fan out from a base image, or care about cold-start latency.
- You want one clean lifecycle for both clone seeds and user-facing snapshots (the same VolumeSnapshot machinery powers both — see [`docs/snapshots/csi-snapshots.md`](../snapshots/csi-snapshots.md)).

## How to opt in

```yaml
apiVersion: image.kubeswift.io/v1alpha1
kind: SwiftImage
metadata: {name: ubuntu-noble-fast}
spec:
  format: qcow2
  rootDisk: {size: "10Gi"}
  cloneStrategy: snapshot
  volumeSnapshotClassName: csi-longhorn-snapclass
  source:
    http: {url: https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img}
```

The validation webhook enforces:

- `cloneStrategy: snapshot` requires `volumeSnapshotClassName`.
- `cloneStrategy: copy` (or empty) rejects `volumeSnapshotClassName`.
- `cloneStrategy` is **immutable once import has progressed past `Pending`**. Switching mid-import leaves the prepared PVC in an ambiguous state.

## State machine

For `cloneStrategy: snapshot`, SwiftImage gains an extra phase:

```
Pending → Importing → Validating → Preparing → Snapshotting → Ready
                                              └─ copy strategy: skips here directly to Ready
```

`Snapshotting` creates a deterministic clone-seed `VolumeSnapshot` named `<image-name>-clone-seed`. When `readyToUse=true`, `status.cloneSeed` is populated and the image flips to `Ready`.

## Per-guest cloning behaviour

Once the image is `Ready`:

- **Copy path**: per-guest PVC created at the SwiftGuestClass `rootDisk.size`, then a Copy Job runs `cp` + `qemu-img resize` + `sgdisk -e`. The PVC is `Bound` and `image.raw` is on disk before the launcher pod is scheduled.
- **Snapshot path**: per-guest PVC created at the **source** size (Longhorn refuses different-size `dataSource` clones — Phase 0 §5) with `dataSource: VolumeSnapshot`. After bind, the controller expands the PVC to target size and **waits for `status.capacity == target`** before scheduling the launcher pod (the expand-and-wait gate; without it, the launcher's `qemu-img resize` would write past the underlying block device end). A `clone-grow-init` init container then runs `qemu-img resize` + `sgdisk -e` once the launcher pod is scheduled.

## Lifecycle and finalizers

When `cloneStrategy: snapshot`, both the SwiftImage and its clone-seed VolumeSnapshot carry the `kubeswift.io/clone-seed-protected` finalizer. Deletion of either resource is blocked until **no SwiftGuests in the same namespace still reference the image**. This is load-bearing for true copy-on-write CSI drivers (Rook Ceph, EBS) where deleting the seed mid-clone corrupts active clones; defensive on full-copy drivers (Longhorn) where the same operation is non-disruptive.

## Storage class compatibility matrix

The behaviour you observe depends on what your CSI driver does for `VolumeSnapshot` + `dataSource: VolumeSnapshot`. KubeSwift surfaces the data-plane semantics; it does not abstract them.

| CSI driver | Snapshot semantics | Clone semantics | Speedup vs copy |
|---|---|---|---|
| **Longhorn (validated)** | Block-level snapshot | Full-copy from snapshot at clone time | None: slower than `copy` (0.5× for one guest, 0.9× for the last of four). Keep `copy`. |
| **Rook Ceph RBD** (validated 2026-08-16) | RBD snapshot (CoW) | RBD clone (CoW) | ≈2× for one guest, ≈2.7× for four (to the guest's IP); the clone PVC binds with no data movement. **Filesystem root disks only — see the warning below.** |
| **AWS EBS** (untested in Phase 0) | EBS snapshot | EBS clone (CoW with hydration) | Expected: large (no full data copy) |
| **GCE PD** (untested in Phase 0) | PD snapshot | PD clone | Expected: large |
| **local-path / NFS / hostPath** | Not supported — must use `cloneStrategy: copy` | n/a | n/a |

> **Rook Ceph RBD: `snapshot` is unsafe for a `volumeMode: Block` root disk.**
> The SwiftImage import PVC holds the disk as a *file inside* an ext4
> filesystem, so a volume snapshot captures the filesystem, not the disk. Ceph
> accepts the Filesystem→Block mode change (it honours
> `allow-volume-mode-change`), the PVC binds with **no error**, and
> `clone-grow-init`'s `sgdisk -e` then writes a fresh empty GPT over it. The
> guest reaches `Running` and never boots. Use `cloneStrategy: copy` for any
> Block root disk — which includes every RWX+Block class used for live
> migration. Filesystem→Filesystem clones are unaffected and boot normally.

If your driver isn't listed and you validate it, please send a PR adding a row.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Image stuck in `Snapshotting` | VolumeSnapshotClass missing or wrong driver | `kubectl describe volumesnapshot <image>-clone-seed` |
| Snapshot ready, but per-guest PVCs stuck `Pending` | Cross-namespace `dataSourceRef` (k0s 1.34 silent fail) | Check that source PVC and SwiftImage are in the SwiftGuest's namespace |
| Per-guest PVC `Bound` but capacity stays at source size | CSI driver doesn't support online expansion | Use `cloneStrategy: copy` for now |
| Webhook rejects `cloneStrategy: snapshot` | `volumeSnapshotClassName` empty | Set the field |
| Webhook rejects `cloneStrategy` change | SwiftImage already past `Pending` | Delete and recreate the SwiftImage with the new strategy |

## Reference

- CSI snapshot/restore for end users: [`docs/snapshots/csi-snapshots.md`](../snapshots/csi-snapshots.md)
