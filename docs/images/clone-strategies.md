# SwiftImage Clone Strategies

> Audience: KubeSwift operators choosing between `cloneStrategy: copy` and `cloneStrategy: snapshot` for SwiftImages.

## Why this exists

When a SwiftGuest boots from a SwiftImage, KubeSwift produces a per-guest root-disk PVC seeded from the image. Two strategies are supported:

| Strategy | How clones are produced | Drivers it works on |
|---|---|---|
| `copy` (default) | Per-guest Copy Job: `cp` from the SwiftImage PVC to the new PVC, then `qemu-img resize` + `sgdisk -e` | Any CSI driver, including non-snapshot-capable ones (local-path, NFS, hostPath) |
| `snapshot` | CSI VolumeSnapshot of the SwiftImage PVC + per-guest `dataSource: VolumeSnapshot` clones, expand-and-wait gate before guest pod schedule | Snapshot-capable CSI drivers (Longhorn, Rook Ceph, EBS, GCE PD) |

The `snapshot` strategy is substantially faster on most snapshot-capable drivers because it skips the per-guest `cp` (≈8–12 GiB read + write for Ubuntu Noble) and the Copy Job's `apt-get install qemu-utils`. On true copy-on-write drivers (Rook Ceph, EBS, GCE PD) the speedup is dramatic; on full-copy drivers like Longhorn it is real but smaller (Phase 0 spike measured ≈3× on a 4 GiB source, more on larger sources).

The default is **`copy`** for backward compatibility. Existing SwiftImages keep working unchanged.

## Choosing a strategy

Use **copy** when:
- Your storage class is local-path / NFS / hostPath — anything without VolumeSnapshot support.
- You only run a handful of guests per image and start-up speed isn't a hot path.
- You want behaviour identical to KubeSwift v0.1.

Use **snapshot** when:
- Your storage class is backed by a snapshot-capable CSI driver.
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
| **Longhorn (validated)** | Block-level snapshot | Full-copy from snapshot at clone time | ≈3–10× depending on source size |
| **Rook Ceph RBD** (untested in Phase 0) | RBD snapshot (CoW) | RBD clone (CoW) | Expected: very large (instantaneous clones) |
| **AWS EBS** (untested in Phase 0) | EBS snapshot | EBS clone (CoW with hydration) | Expected: large (no full data copy) |
| **GCE PD** (untested in Phase 0) | PD snapshot | PD clone | Expected: large |
| **local-path / NFS / hostPath** | Not supported — must use `cloneStrategy: copy` | n/a | n/a |

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

- Source design and decision history: [`docs/design/snapshots.md`](../design/snapshots.md) (search for "SwiftImage Clone Strategy")
- Phase 0 driver findings: [`docs/design/snapshots-spike-results.md`](../design/snapshots-spike-results.md) §5–§6
- CSI snapshot/restore for end users: [`docs/snapshots/csi-snapshots.md`](../snapshots/csi-snapshots.md)
