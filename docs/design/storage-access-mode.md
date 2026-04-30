# Storage Access Mode Selection

> Status: shipped — KubeSwift PR #32
> Last updated: 2026-04-30
> Replaces: live-migration-phase-2 §5.1.2 "RWX rejected" framing

## Why this exists

Phase 2 of the live-migration spike documented "RWO required" as a load-bearing
constraint of the manual demo. Post-merge mini-walkthrough surfaced finding W6:
the constraint as written is wrong for the broader feature. Live migration of
disk-boot guests requires RWX+Block (the KubeVirt model) OR a not-yet-shipped
RWO-handoff choreography in Phase 3. Phase 2 sidestepped the question because
the spike's reference workload was a kernel-boot guest with no PVC at all —
the disk-handoff problem was invisible.

This PR is the **API-surface unblock**. It does not resolve the deeper storage
architecture review (separate, follow-on work). What it ships:

- Operators can declare per-class (and per-guest override) the storage access
  mode and volume mode for SwiftGuest disks
- The CRD admission webhook rejects the RWX+Filesystem combination at submit
  time (CRD-level CEL rule) — operators cannot accidentally claim live-
  migration capability the storage layer cannot deliver
- The SwiftMigration validating webhook gains a forward-compatible live-mode
  storage gate that fires once Phase 3 lands
- Per-driver pre-flight checks (today: Longhorn migratable parameter) surface
  through `status.conditions[StorageReady]` — informational, NOT an
  admission gate

Defaults preserve current behaviour: ReadWriteOnce + Filesystem +
storageClassName inherited from the source SwiftImage's PVC.

## CRD shape

```yaml
# SwiftGuestClass — cluster-scoped default
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestClass
spec:
  cpu: "2"
  memory: "4Gi"
  rootDisk:
    size: "40Gi"
    format: raw
  storage:                              # optional; defaults below
    accessMode: ReadWriteOnce | ReadWriteMany   # default ReadWriteOnce
    volumeMode: Filesystem | Block               # default Filesystem
    storageClassName: <string>                   # default empty (inherit)
```

```yaml
# SwiftGuest — namespaced; spec.storage overrides class per-field
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
spec:
  guestClassRef: { name: small-rwo }
  storage:                              # optional; per-field merge
    accessMode: ReadWriteMany           # overrides class
    volumeMode: Block                   # overrides class
    storageClassName: longhorn-migratable
status:
  storage:                              # informational echo
    accessMode: ReadWriteMany
    volumeMode: Block
    storageClassName: longhorn-migratable
```

`status.storage` is an **informational echo** of the resolved spec, written by
the SwiftGuest controller on every reconcile. There is **no
liveMigrationCapable field in status** — derived facts in status race
controller-write-back during cluster restore (the SwiftMigration webhook
would observe pre-reconcile false negatives and reject valid migrations). The
canonical capability rule (RWX+Block) is recomputed at admission time by
both the webhook and `swiftctl describe`.

## Resolution rules

Per-field merge: each non-empty field on `SwiftGuest.spec.storage` wins, then
the same field on `SwiftGuestClass.spec.storage`, then system defaults.

| Layer | accessMode | volumeMode | storageClassName |
|---|---|---|---|
| Default | ReadWriteOnce | Filesystem | "" (inherit from source SwiftImage's PVC) |
| Class | resolves field by field | | |
| Guest | wins on any non-empty field | | |

`storageClassName` is `*string` (pointer to string) so nil ("not set; fall
through") is distinguishable from "" ("explicit cluster default"). Both
currently resolve to the empty string in the resolved spec, but the
distinction is preserved for forward compatibility.

## Validation

### CRD admission (CEL)

The `StorageSpec` struct carries an OpenAPI v1 `XValidation` rule:

```
self.accessMode != 'ReadWriteMany' || self.volumeMode == 'Block'
```

`kubectl apply` — including `kubectl apply --dry-run=server` — rejects
RWX+Filesystem manifests offline.

Why HARD reject (not warning + status condition):

- Filesystem RWX (Longhorn Generic RWX, NFS-based) is multi-node
  share-friendly but **not live-migration-capable**. Operators reading
  `accessMode: ReadWriteMany` reasonably read it as "live-migratable."
- Discovering the gap at drain time — the worst possible moment — produces
  the worst operator experience. Strict at submit time, relax later if the
  data-disk path needs RWX+FS for application-level sharing.
- Future data disks (out of scope here) will get their own per-disk storage
  block; data-disk RWX+FS will be permitted there because it doesn't
  advertise root-disk migration capability.

### SwiftGuest webhook (cross-resource — none today)

The CRD rule covers all spec-shape concerns. The SwiftGuest validating
webhook stays focused on its current responsibilities (image+kernel
mutual exclusion, etc.).

### SwiftMigration webhook (live-mode gate, ValidateCreate only)

In `validateClusterState`, after the existing GPU and networking gates, the
webhook recomputes the resolved storage spec from the source guest + class
and rejects mode=live for non-RWX+Block guests.

```go
if mig.Spec.Mode == migrationv1alpha1.SwiftMigrationModeLive {
    if err := v.gateLiveModeStorage(ctx, &guest); err != nil {
        return nil, err
    }
}
```

Phase 1 already rejects mode=live at `validateShape` (the "live not yet
shipped" rule). The storage gate is forward-compatible: when Phase 3 lands
and `validateShape` accepts mode=live, the storage gate is what stops
operator submissions on incapable storage.

Per PR #26's per-operation discipline, the gate fires on `ValidateCreate`
only. `ValidateUpdate` is shape-only (spec is immutable; cluster-state
revalidation has no value and turns transient cluster conditions into
stuck resources). `ValidateDelete` is pass-through.

## Per-driver pre-flight checks (status condition)

The SwiftGuest controller writes a `StorageReady` condition on every
reconcile. The check fires only when the resolved storage is
live-migration-capable (RWX+Block):

| Provisioner | Behaviour |
|---|---|
| `driver.longhorn.io` | Read StorageClass; require `parameters.migratable: "true"`. Missing → `StorageReady=False`, reason `LonghornNotMigratable`, message names the cluster-admin remedy. |
| Other CSI drivers | Pass through. Future per-driver checks (Ceph RBD `imageFeatures`, EBS volume types) land alongside Longhorn. |
| `storageClassName` empty | Defer (the resolved class falls through to the source SwiftImage's PVC class — invisible at this layer). No false alarm. |
| StorageClass missing | `StorageReady=False`, reason `StorageClassNotFound`. PVC bind would fail anyway; surfacing the cause earlier helps. |

The condition is **informational** — it does not gate pod creation. Cluster
admins can fix the StorageClass (or apply one) and the SwiftGuest reconciles
to `StorageReady=True` on the next sweep without restart.

## Sample manifests

`config/samples/storage/`:

- `swiftguestclass-rwo.yaml` — explicit RWO+Filesystem (matches the
  default; provided for documentation clarity)
- `swiftguestclass-rwx-migratable.yaml` — RWX+Block class plus a sample
  Longhorn StorageClass with `migratable: "true"`
- `swiftguest-rwx-override.yaml` — single-guest RWX+Block override of an
  RWO class

## Reference points

- KubeVirt's RWX-required-for-live-migration model: KubeVirt requires
  RWX storage for live migration. Non-RWX VMs are not live-migratable
  by design; libvirt/QEMU coordinate exclusive write access during
  migration to avoid the F2 split-brain class of issue.

- Longhorn 1.11.1 RWX modes:
  - **Generic (Non-Migratable) RWX** — NFS-based, multi-node file
    access. NOT live-migration-capable. Filesystem volumeMode.
  - **Migratable RWX** — block-mode, designed for KubeVirt-style
    live migration. Requires StorageClass parameter `migratable: "true"`.
    Block volumeMode.

  KubeSwift's CRD surface is driver-agnostic (`accessMode` and `volumeMode`
  are Kubernetes primitives). Per-driver translation lives in the
  controller's small adapter (`internal/controller/swiftguest/storage.go`).

## Open questions deferred to storage architecture review

This PR does not resolve, and explicitly defers to the storage architecture
review:

1. CSI driver matrix: Longhorn is validated; Ceph RBD, AWS EBS, and other
   drivers need per-driver pre-flight checks before they're production-
   ready for KubeSwift live migration.
2. F2 split-brain mitigation on RWX: KubeVirt's libvirt/QEMU coordination
   isn't directly portable to Cloud Hypervisor; CH's behaviour on
   concurrent RWX writes is not yet validated. Phase 3 must address
   this in the StopAndCopy phase.
3. Default for new clusters: should the cluster-installer-shipped default
   SwiftGuestClass declare RWX+Block where the underlying driver supports
   it, or stay RWO+Filesystem and require operators to opt in?
4. Data disks: when controller-created data disks land (currently only
   `dataDiskRef`/`dataDiskRefs` referencing pre-existing PVCs/SwiftImages),
   does data-disk storage spec compose with root-disk storage spec or live
   on the data-disk entry independently? The architect's call (PR #32
   sub-agent review): independently, with class-level fall-through. Code
   landing this is out of scope.

## Migration path

- **No-op for existing manifests.** SwiftGuestClasses without a `storage`
  block resolve to RWO+Filesystem+inherit (the pre-PR-32 behaviour).
  SwiftGuests likewise.
- **CRD admission for new manifests.** RWX+Filesystem is rejected at
  submit time; operators see the error with a docs cross-reference.
- **swiftctl describe** shows the resolved spec and the recomputed
  `LiveMigrationCapable` boolean.
- **No data migration required.** Existing PVCs keep their existing
  access mode and volume mode; the change is per-PVC at creation time.

## Cross-references

- `api/swift/v1alpha1/storage_types.go` — `StorageSpec`,
  `ResolvedStorageStatus`, `IsLiveMigrationCapable`
- `internal/resolved/merge.go::MergeStorage` — per-field merge, exported
  for the SwiftMigration webhook
- `internal/controller/swiftguest/storage.go` — PVC field helpers +
  Longhorn pre-flight check
- `internal/webhook/swiftmigration/validator.go::gateLiveModeStorage` —
  Phase 3 forward-compat live-mode gate
