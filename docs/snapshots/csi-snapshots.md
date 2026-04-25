# CSI VolumeSnapshot Snapshots and Restores

> Audience: KubeSwift operators
>
> Status: Phase 1 (csi-volume-snapshot backend). Local and S3 backends are reserved for later phases — see [`docs/design/snapshots.md`](../design/snapshots.md).

## What you get

KubeSwift Phase 1 ships disk-only, crash-consistent VM snapshots backed by `snapshot.storage.k8s.io/VolumeSnapshot`:

- **`SwiftSnapshot`** captures a SwiftGuest's per-guest root disk PVC at a point in time.
- **`SwiftRestore`** materialises a SwiftSnapshot as a new SwiftGuest, copying the source guest's spec.

The VM is **not paused** during snapshot. The capture is crash-consistent — equivalent to a hard reboot at restore time. Memory snapshots are out of scope for Phase 1; the Phase 0 spike showed Cloud Hypervisor's snapshot/restore is incompatible with VFIO and unsuitable as a Phase 1 baseline (see [snapshots-spike-results.md](../design/snapshots-spike-results.md) §2/§3).

## Prerequisites

- Snapshot-capable CSI driver (Longhorn, Rook Ceph, EBS, GCE PD, …) with the `external-snapshotter` controller installed.
- A `VolumeSnapshotClass` reachable to the source PVC's StorageClass.
- KubeSwift controller-manager running with snapshot-related RBAC (the Helm chart wires this in v0.2+).

## Take a snapshot

```bash
swiftctl snapshot create db-2026-04-25 --guest db --vsclass csi-hostpath-snapclass
```

The `--vsclass` flag is optional when the cluster has a default VolumeSnapshotClass (annotation `snapshot.storage.kubernetes.io/is-default-class=true`).

Equivalent CR:

```yaml
apiVersion: snapshot.kubeswift.io/v1alpha1
kind: SwiftSnapshot
metadata:
  name: db-2026-04-25
  namespace: prod
spec:
  guestRef: {name: db}
  backend:
    type: csi-volume-snapshot
    csiVolumeSnapshot:
      volumeSnapshotClassName: csi-hostpath-snapclass
```

State machine: `Pending → Capturing → Ready` (or `Failed`).

`Pending` validates the source guest exists and its per-guest root-disk PVC is bound; `Capturing` creates the underlying `VolumeSnapshot` and waits for `readyToUse=true`; `Ready` records the disk handle, size, and captured-at timestamp.

Inspect:

```bash
swiftctl snapshot describe db-2026-04-25
swiftctl snapshot list -A
kubectl get volumesnapshot swift-snap-db-2026-04-25 -n prod
```

## Restore a snapshot into a new VM

```bash
swiftctl restore create r1 --snapshot db-2026-04-25 --target db-restored
```

Optional flags:

- `--no-resume` — leave the restored guest in `runPolicy=Stopped` for inspection.
- `--overwrite-existing` — replace an existing SwiftGuest at the target name.

Equivalent CR:

```yaml
apiVersion: snapshot.kubeswift.io/v1alpha1
kind: SwiftRestore
metadata:
  name: r1
  namespace: prod
spec:
  snapshotRef: {name: db-2026-04-25}
  targetGuest: {name: db-restored}
  resumeAfterRestore: true
```

State machine: `Pending → Restoring → Resuming → Ready`. With `resumeAfterRestore=false`, `Resuming` is skipped.

The controller copies the source guest's spec to the target, pre-creates the target's per-guest root-disk PVC sourced from the snapshot's VolumeSnapshot (label `swift.kubeswift.io/restore-seeded=true` so the SwiftGuest controller skips the Copy Job), and finally creates the SwiftGuest. The source SwiftGuest must still exist at restore time — its spec is read by the controller.

## Constraints

- **Same-namespace.** SwiftSnapshot, SwiftRestore, the source guest, the target guest, and the underlying VolumeSnapshot all live in the same namespace. Cross-namespace references are not expressible (the API has no `namespace` field on `guestRef` / `snapshotRef` / `targetGuest`). This is structural, not configurable — Phase 0 spike §6a showed cross-namespace `dataSourceRef` silently fails on k0s 1.34.
- **Disk-only, crash-consistent.** Memory state is not captured. Quiesce databases (`fsfreeze`, application-level checkpoints) before snapshotting if you need application consistency.
- **Source size.** The clone PVC is provisioned at the snapshot's source size and expanded post-bind if the SwiftGuestClass requests more — Longhorn refuses different-size dataSource clones (Phase 0 §5).
- **Backend allowlist.** Only `csi-volume-snapshot` is implemented; the validation webhook rejects `local` and `s3` backends with a clear "reserved for Phase N" message.
- **Spec immutability.** Both SwiftSnapshot and SwiftRestore specs are immutable after creation. Re-running with different inputs creates a new resource.
- **Identity regeneration.** `spec.identity.regenerate` is reserved for Phase 2 (memory-snapshot clones); it must be empty in Phase 1.

## Cleanup

A SwiftSnapshot is protected by the `kubeswift.io/clone-seed-protected` finalizer when its underlying VolumeSnapshot is referenced as a SwiftImage clone seed (see [Clone strategies](../images/clone-strategies.md)). Manual SwiftSnapshots created via `swiftctl snapshot create` are safe to delete directly:

```bash
swiftctl snapshot delete db-2026-04-25
```

The controller deletes the underlying VolumeSnapshot via owner reference. The CSI external-snapshotter then garbage-collects the VolumeSnapshotContent and provider-side state.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `Phase=Pending` with `Ready=False reason=GuestNotFound` | Source guest deleted or wrong namespace | Recreate the snapshot once the source exists |
| `Phase=Pending` with `Ready=False reason=RootPVCNotFound` | SwiftGuest still provisioning | Wait — the per-guest PVC takes 1–2 min to create on cold start |
| `Phase=Failed reason=SnapshotFailed` | CSI driver returned a snapshot error | `kubectl describe volumesnapshot swift-snap-<name>` for the driver message |
| `Phase=Failed reason=UnsupportedBackend` | `backend.type` is `local` or `s3` | Phase 1 only — use `csi-volume-snapshot` |
| Restore `Phase=Failed reason=TargetConflict` | Target name already in use | Set `targetGuest.overwriteExisting=true` or pick a different name |
| Restore `Phase=Pending reason=SnapshotNotReady` | Source SwiftSnapshot not yet `Ready` | Wait for the snapshot to complete first |

## Reference

- Source design: [`docs/design/snapshots.md`](../design/snapshots.md)
- Phase 0 spike findings: [`docs/design/snapshots-spike-results.md`](../design/snapshots-spike-results.md)
- Clone-strategy bundling (uses the same VolumeSnapshot machinery internally): [`docs/images/clone-strategies.md`](../images/clone-strategies.md)
