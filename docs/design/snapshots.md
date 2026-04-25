# VM Snapshots — Design

> **Status:** Design — not yet implemented
> **Audience:** KubeSwift maintainers, AI assistants picking up this work
> **Last updated:** April 25, 2026
> **Target file path in repo:** `docs/design/snapshots.md`

---

## Purpose

This document describes how KubeSwift will support VM snapshots, covering all
three of the canonical use cases — backup/restore, cloning/templating, and
migration prep — with an explicit storage-backend tier model. It is split into:

1. **Concepts** — what we're building and why, the constraints, the tier model
2. **Work Plan** — phased implementation tasks ready to execute

When picking this up, read the Concepts section in full first. Several
fundamental constraints (especially around VFIO and Cloud Hypervisor's
snapshot/restore API) shape the entire design and must be understood before
any implementation work begins.

---

# Part 1 — Concepts

## Goal

Enable KubeSwift operators to snapshot a virtual machine's complete state
(memory + disk) and restore it later, on the same node or a different one,
producing a VM identical to the snapshotted instance. The same primitives
must support backup/restore workflows, cloning a snapshot into new VMs
(i.e. templating), accelerating SwiftImage-to-SwiftGuest cloning so pool
scaling is fast, and serving as the foundation for live migration in a
later phase.

The CSI VolumeSnapshot infrastructure built for user-facing snapshots is
also reused internally by SwiftImage to make new VM creation fast — this
is treated as plumbing rather than a separate feature, and is documented
in the "SwiftImage Clone Strategy" section.

## Hard Constraints (read this section twice)

These constraints come from Cloud Hypervisor itself, not from KubeSwift. They
cannot be designed around — only worked with.

### Constraint 1 — VFIO is incompatible with memory snapshots

Cloud Hypervisor's snapshot/restore feature **does not support VFIO
passthrough devices**. This has been the case since the feature was introduced
in v0.8.0 and remains true as of v51.x.

**The failure mode is on restore, not on snapshot.** This is critical and
was confirmed by Phase 0 spike testing on CH v51.1: Cloud Hypervisor will
silently and successfully produce a complete snapshot directory for a VM
with VFIO devices attached. No error is surfaced at snapshot time. The
failure surfaces only when the snapshot is restored, at which point Cloud
Hypervisor's device manager fails with a clear error chain:

```
Error restoring VM
  → The VM could not be restored
  → Error from device manager
  → Cannot allocate PCI BARs
  → Registering an IO BAR failed
  → bar 0 already used
```

**Implication for Phase 2 controllers:** the snapshot controller MUST
reject memory-snapshot creation up-front for VFIO VMs based on the
SwiftGuest's spec (`gpuProfileRef` set, or any interface with
`type: sriov`). We cannot rely on Cloud Hypervisor to surface the error
at snapshot time — operators would otherwise produce broken snapshots
that only fail at restore time, possibly months later during disaster
recovery.

This means VFIO workloads (memory-snapshot-incompatible):

- **Tier 1 GPU VMs (PCIe passthrough)** — cannot be memory-snapshotted
- **Tier 2 GPU VMs (HGX SXM with NVSwitch)** — cannot be memory-snapshotted
- **Tier 3 GPU VMs (full HGX passthrough)** — cannot be memory-snapshotted
- **SR-IOV NIC passthrough VMs** — cannot be memory-snapshotted

These workloads can still get **disk-only snapshots** via Tier A (CSI
VolumeSnapshot). That capability is preserved through the tier model below.

### Constraint 2 — Snapshot requires the VM to be paused

Cloud Hypervisor requires `pause` before `snapshot`. The VM is unresponsive
during the snapshot capture. This is downtime, however brief. There is no
"live" snapshot in the QEMU/libvirt sense for Cloud Hypervisor today.

KubeSwift's snapshot operation must:

1. Pause the VM (network and CPU stop)
2. Capture the snapshot to a destination
3. Resume the VM (or leave paused if requested)

The pause window is short (seconds to tens of seconds depending on VM memory
size and storage speed) but is real downtime. This must be communicated
clearly to operators.

### Constraint 3 — Restore produces a paused VM

After a restore, the VM is in the paused state. It must be explicitly
resumed. This is not a bug — it lets operators verify state before the VM
starts running. KubeSwift will handle the resume automatically by default
with an option to leave paused.

### Constraint 4 — Snapshot directory layout is opaque

Cloud Hypervisor writes a snapshot to a directory containing multiple files
(VM config JSON, memory regions, device states). The exact layout is internal
and may change across CH versions. KubeSwift must:

- Treat the snapshot directory as an opaque blob from the user's perspective
- Record the CH version that produced the snapshot
- Refuse to restore a snapshot with an incompatible CH version (or warn loudly)

### Constraint 5 — QEMU runtime path needs separate snapshot handling

KubeSwift uses QEMU for Tier 2/3 GPU workloads. Those workloads cannot be
memory-snapshotted anyway (VFIO constraint). For non-GPU QEMU workloads
(if any exist in the future) snapshot support would use QEMU's own
`savevm`/`loadvm` API via QMP, which is a separate implementation path.

For the initial design, **memory snapshots are Cloud-Hypervisor-only**.
QEMU-backed VMs only get disk-only snapshots.

## Use Case 1 — Backup and Restore

Operator wants point-in-time recovery of a VM. Use cases include
pre-upgrade safety nets, data corruption recovery, and disaster recovery.

**Workflow:**
```
SwiftSnapshot created (referencing a SwiftGuest)
  → controller pauses VM via CH API
  → captures memory + disk state to storage backend
  → resumes VM
  → SwiftSnapshot.status.phase = Ready

Later: operator creates SwiftRestore (referencing the SwiftSnapshot)
  → controller stops the original VM
  → restores from snapshot into a new launcher pod
  → VM resumes in the previously snapshotted state
```

The original SwiftGuest may or may not exist at restore time. Restore can
target a new SwiftGuest name.

## Use Case 2 — Cloning and Templating

Operator wants to create new VMs from a known-good state. Use cases include
golden-image workflows, fast VM provisioning, and CI/CD where each test run
starts from a clean snapshot.

**Workflow:**
```
SwiftSnapshot created (as above)
  → captured

Later: operator creates one or more SwiftGuests with
       spec.cloneFromSnapshot pointing at the SwiftSnapshot
  → each guest restores from the snapshot
  → controller assigns each a unique MAC, hostname, network identity
  → each VM resumes independently
```

This is a pattern that real operators want and that today's "boot from
SwiftImage" path cannot serve well — boot from cloud image takes minutes,
clone from snapshot takes seconds.

**Identity divergence**: cloned VMs share state at clone time, including
hostname, machine-id, SSH host keys, and DHCP client identifier. The clone
mechanism must regenerate these (or rely on cloud-init `manage_etc_hosts`,
`ssh_genkeytypes`, etc., on first boot of the clone). For memory clones,
this is harder than for disk clones because cloud-init has already run.
**This is the central technical risk in the cloning use case** and gets
its own section below.

## Use Case 3 — Migration Prep

Operator wants to move a running VM from one host to another with minimal
downtime. Use cases include node maintenance, rebalancing, and hardware
failure response.

**Workflow:**
```
For pause-snapshot-resume migration (offline-but-fast):
  → pause source VM
  → snapshot to shared storage
  → start launcher pod on destination node
  → restore from snapshot
  → original launcher pod terminated
```

This is **offline migration** — the VM is down for the duration of the
snapshot capture and restore. It is not the same as live migration (which
ships memory pages while the VM keeps running). Offline migration is what
falls out for free from the snapshot infrastructure. Live migration is a
separate, larger design.

For disk migration alone (VM moves to a node with the same shared storage),
this is much simpler and degenerates to "stop, schedule, start" without
needing memory transfer at all.

## Use Case 4 — Fast SwiftImage Cloning (Internal Acceleration)

This use case is operator-invisible: operators never directly create snapshots
for it. They simply experience faster VM creation, especially for
SwiftGuestPool scaling.

**The problem today:** when a SwiftGuest is created, the existing
per-guest root disk cloning architecture creates a new PVC by running a
Copy Job that does `cp image.raw clone.raw`, then `qemu-img resize`, then
`sgdisk -e`. For a 40GB Ubuntu cloud image this takes several minutes per
VM, even on local NVMe. For a SwiftGuestPool scaling from 5 to 50 replicas,
this becomes the dominant time cost — pool members sit in `Pending` waiting
for storage I/O.

**The fix:** when the underlying CSI driver supports VolumeSnapshot,
KubeSwift snapshots the SwiftImage's PVC once during import, then creates
each per-guest clone PVC with `dataSource` pointing at that VolumeSnapshot.
CSI drivers handle this with copy-on-write at the storage layer — the new
PVC is bound near-instantly, with no file copy involved.

**Workflow:**
```
SwiftImage created (cloneStrategy: snapshot)
  → import Job downloads + converts to raw + patches GRUB (existing flow)
  → controller creates VolumeSnapshot of image PVC
  → SwiftImage.status.cloneSeed = VolumeSnapshot reference
  → SwiftImage.status.phase = Ready

SwiftGuest created (referencing image)
  → controller checks SwiftImage.status.cloneSeed
  → if VolumeSnapshot: create clone PVC with dataSource → snapshot (CoW, seconds)
  → if nil (legacy): fall back to Copy Job (current behavior)
  → VM boots; cloud-init growpart resizes the partition on first boot
```

**Why this composes with the user-facing snapshot work:** Phase 1 of this
design builds CSI VolumeSnapshot integration for user-facing SwiftSnapshot.
That same plumbing is what SwiftImage uses internally. Building both at
the same time costs marginally more than building one — and operators get
fast pool scaling as a free side effect of shipping snapshots.

**Performance expectation:** with snapshot-based cloning, the storage-layer
clone replaces the userspace `cp` operation. Actual clone time depends on
the CSI driver's clone semantics, which vary substantially:

- **True copy-on-write drivers** (Rook Ceph, AWS EBS, GCE PD): clone is
  near-instant. PVC binds and the pod can attach immediately. Pool scaling
  is bounded by Kubernetes scheduling and cloud-init.
- **Full-copy drivers** (Longhorn): the PVC reaches `Bound` quickly
  (~3 seconds), but the underlying data copy runs in the background and
  the pod cannot attach until the copy completes. Empirical measurement
  on Longhorn v1.11.1 with a 10 GiB image: ~100 seconds per clone for the
  background copy. Pool scaling on Longhorn is therefore bounded by per-
  replica clone time, not by Kubernetes scheduling.

In all cases, snapshot-based cloning is **substantially faster than the
legacy Copy Job path** because the storage driver handles the copy more
efficiently than userspace `cp` (replica-aware, sparse-aware, parallel
within the storage cluster). On Longhorn this is roughly a 3-4× speedup;
on true CoW drivers it can be 50-100× speedup.

The user-facing documentation must list validated CSI drivers and their
clone semantics so operators can set realistic expectations for their
specific cluster.

The full design for this is in the **SwiftImage Clone Strategy** section
below.

## Storage Backend Tier Model

Snapshots are large. Disk state alone can be tens to hundreds of GB. Memory
state adds another VM-memory worth (potentially hundreds of GB for GPU
workloads). Where the snapshot lives matters for correctness, performance,
and cluster portability.

This design supports three tiers, picked per snapshot via a backend field on
the SwiftSnapshot resource. Operators choose based on their cluster
capabilities.

### Tier A — CSI VolumeSnapshot (disk-only)

**What:** Use Kubernetes' standard `VolumeSnapshot` API to snapshot the
disk PVCs (root disk, data disks).

**Memory state:** Not captured. VM is fully shut down before snapshot.

**Pros:**
- Standard, vendor-neutral Kubernetes API
- CSI driver handles efficient snapshotting (CoW, COW deltas, dedup)
- Works across nodes if the CSI driver supports it
- Backup tools (Velero, Kasten, etc.) integrate natively

**Cons:**
- Disk-only — no in-memory state preserved
- Requires the CSI driver to support snapshots (not all do — `local-path` does
  not, `longhorn` does, `rook-ceph` does, cloud CSI drivers do)
- Snapshot is per-PVC, so multi-disk VMs need application-level coordination
  for consistency (or VM shutdown)

**When to use:** Standard backup workflows, GPU/SR-IOV VMs (which cannot
have memory snapshots anyway), clusters with a CSI snapshot-capable driver.

**KubeSwift implementation:** Stop VM, create `VolumeSnapshot` resources
referencing the root PVC and any data PVCs, wait for `readyToUse=true`,
record the snapshot handles in `SwiftSnapshot.status`.

### Tier B — Local copy (memory + disk)

**What:** Capture Cloud Hypervisor's snapshot directory to a hostPath
volume on the node, plus copy disk artifacts to the same hostPath.

**Memory state:** Captured.

**Pros:**
- Works on any cluster, any storage class
- Fast on local NVMe
- No CSI dependency

**Cons:**
- Snapshot lives on a single node — if the node fails, the snapshot is gone
- Restore is locked to the same node (or requires manual copy)
- HostPath has security implications (privileged access)
- Disk space on the node is the limit

**When to use:** Single-node clusters, lab/dev environments, fast iteration
workflows where snapshot durability is not critical, scenarios where the
operator wants to take a quick checkpoint and revert minutes later.

**KubeSwift implementation:** Pause VM via CH API, write snapshot to
`/var/lib/kubeswift/snapshots/<namespace>-<name>/`, copy the disk file(s)
to the same directory, resume VM, record paths in `SwiftSnapshot.status`.

### Tier C — S3 / object storage export (memory + disk)

**What:** Capture the snapshot to local storage, then upload to S3-compatible
object storage. Restore pulls from S3 to a local cache, then restores from
local cache.

**Memory state:** Captured.

**Pros:**
- Cluster-portable (any node can pull from S3)
- Durable, off-cluster storage
- Cheap for cold storage of many snapshots
- Aligns with backup tooling that already targets S3

**Cons:**
- Slower than Tier A or B (network bandwidth bound)
- Requires credentials management (Secret with S3 access keys)
- More moving parts to fail
- Upload/download time dominates the operation

**When to use:** Production backup workflows that need cross-node and
off-cluster durability, long-term retention, regulatory compliance scenarios.

**KubeSwift implementation:** Tier B's local capture step, then a separate
upload Job that pushes to S3 using credentials from a referenced Secret.
On restore, a download Job pulls from S3 first.

### Tier choice matrix

| Use case | VM type | Recommended tier |
|----------|---------|------------------|
| Backup of stateful Linux VM | non-GPU | Tier A (preferred) or Tier C |
| Backup of GPU/SR-IOV VM | passthrough | Tier A only (no memory option) |
| Quick local checkpoint | non-GPU | Tier B |
| Production cross-node DR | non-GPU | Tier C |
| Templating golden VM | non-GPU | Tier B (then promote to C for distribution) |
| Templating GPU VM | passthrough | Tier A only (cold-clone, runs cloud-init on first boot) |
| Migration prep | non-GPU | Tier B (same-storage) or Tier C (cross-storage) |
| Migration prep | passthrough | Not supported via memory snapshot — disk-only via Tier A |

The tier model is deliberately parallel to the GPU tier model. Operators
already understand "the harder thing requires more setup."

## SwiftImage Clone Strategy

This section describes the internal acceleration mechanism introduced in
Use Case 4. It is plumbing — operators do not interact with it directly
beyond choosing a clone strategy at SwiftImage creation time (and they
can leave the default).

### Field on SwiftImage

```yaml
apiVersion: image.kubeswift.io/v1alpha1
kind: SwiftImage
spec:
  source:
    http:
      url: https://cloud-images.ubuntu.com/noble/current/...
  format: qcow2
  cloneStrategy: snapshot         # copy | snapshot | thin (future)
  cloneStorageClassName: longhorn # optional — defaults to image PVC's class
  volumeSnapshotClassName: csi-snapshotter  # required when cloneStrategy=snapshot
status:
  phase: Ready
  cloneSeed:                      # populated when ready
    kind: VolumeSnapshot          # VolumeSnapshot | nil (copy mode)
    name: ubuntu-noble-source
    namespace: kubeswift-system
```

### The three strategies

**`copy` (default for backward compatibility).** Existing behavior.
SwiftImage import produces a single PVC with `image.raw`. Each SwiftGuest
gets a full file copy via the existing Copy Job. Slow but works on any
storage class, including `local-path-provisioner` and NFS without
snapshot support.

**`snapshot` (the fast path, the headline addition).** After import
completes, the SwiftImage controller creates a CSI VolumeSnapshot of the
image PVC. The snapshot becomes the clone seed. Each SwiftGuest gets a
new PVC with `dataSource` pointing at that VolumeSnapshot. The CSI driver
handles the clone with copy-on-write — typically seconds, not minutes.
Requires a snapshot-capable CSI driver and a VolumeSnapshotClass.

**`thin` (future, optional).** Some CSI drivers support direct
PVC-to-PVC cloning (`PersistentVolumeClaim.dataSource: { kind:
PersistentVolumeClaim }`) more efficiently than going through a snapshot.
This strategy uses that path when supported. Not in the v1 scope; listed
here so the field can be defaulted and extended later without a breaking
change.

### Default behavior

The default for `cloneStrategy` is `copy` for v1 to preserve existing
behavior on clusters that may not have a snapshot-capable CSI driver.
Documentation strongly recommends `snapshot` when the cluster supports
it, and `swiftctl image create` could prompt or autodetect (TBD in
implementation).

A future minor release may flip the default to `snapshot` once it's
been broadly validated, with `copy` as an explicit opt-out.

### Lifecycle: keeping the snapshot alive while clones reference it

The source VolumeSnapshot must persist for as long as any SwiftGuest's
clone PVC depends on it. CSI driver behavior here varies, and Phase 0
spike testing confirmed the variance is real:

- **True copy-on-write drivers** (Rook Ceph, AWS EBS, GCE PD): the clone
  PVC contains a CoW reference back to the snapshot. Deleting the
  snapshot while clones exist would break those clones. Finalizer is
  **load-bearing** — without it, snapshot deletion silently corrupts
  active clones.
- **Full-copy drivers** (Longhorn): the clone PVC is a fully independent
  copy of the snapshot's data once the background copy completes.
  Deleting the source snapshot has no effect on existing clones. Phase 0
  spike testing on Longhorn v1.11.1 with both `Retain` and `Delete`
  deletion policies confirmed the source snapshot can be deleted while
  the clone PVC is actively in use, with no observable disruption to the
  workload using the clone. Finalizer is **defensive** rather than
  load-bearing.

KubeSwift's safe default treats the conservative case as canonical: the
SwiftImage controller adds a finalizer
(`kubeswift.io/clone-seed-protected`) to the source VolumeSnapshot. On
SwiftImage deletion:

1. The controller checks for SwiftGuests still referencing this image.
2. If none exist (or all clone PVCs have been deleted), the finalizer is
   removed and the VolumeSnapshot is allowed to be garbage collected.
3. If clones still exist, deletion blocks until they are gone, with a
   clear status message explaining why.

This is correct across all CSI drivers — load-bearing for true CoW
drivers, defensive for full-copy drivers. The cost on full-copy drivers
is a brief block at SwiftImage delete time, which is the right behavior
regardless. Operators do not need to know which type of driver they're
on; the finalizer handles both correctly.

**Open question (deferred to follow-up spike):** what happens when the
SwiftImage's source PVC (the import artifact, not the snapshot) is
deleted while a snapshot of it has bound clones? Phase 0 deferred this
test for cluster safety reasons. On Longhorn, snapshots are stored
alongside their source volume's replicas, so source PVC deletion may
break the snapshot. On other drivers the behavior may differ. Phase 1
should treat this as an operator responsibility: do not delete a
SwiftImage's source PVC while the SwiftImage is in use. A follow-up
spike before Phase 2 should validate this case across the drivers we
target.

### Disk size handling

Today's flow: Copy Job runs `qemu-img resize` to grow the file to the
SwiftGuestClass `rootDisk.size`, then `sgdisk -e` fixes the GPT backup
header, then VM boots and cloud-init `growpart` extends the root
partition.

With snapshot-based cloning, the question is whether the storage layer
allows creating a clone PVC larger than the source snapshot. Phase 0
spike testing found that **driver behavior varies** and the design must
not assume the optimistic case:

- **Some CSI drivers** (e.g. AWS EBS, GCE PD) accept clone PVCs larger
  than the source snapshot and allocate the requested capacity directly.
  In these cases, no `qemu-img resize` is needed at the file level.
- **Other CSI drivers** (notably Longhorn v1.x) **refuse** dataSource
  clones with target size ≠ source size, returning an HTTP 500 error
  from the provisioner with a message like "size of target volume is
  different than size of source volume." The clone PVC must be created
  at the source's size, then expanded via `kubectl patch` (which takes
  ~50 seconds on Longhorn for a 10→40 GiB expansion).
- **Some drivers** may allow target-size = source-size only when both
  are aligned to specific block sizes; corner cases exist.

To avoid driver-specific code paths in Phase 1, the implementation
**preserves the existing `qemu-img resize + sgdisk -e + growpart` flow**
for snapshot-cloned disks, just as it does for Copy Job-cloned disks.
The snapshot fast-path saves the **data copy** (the slow part — minutes
of `cp` on a 40 GiB image), but does not save the disk-grow steps
(seconds of metadata operations). Net effect:

1. Clone PVC is created at the source snapshot's size via dataSource.
2. The PVC is expanded to the SwiftGuestClass `rootDisk.size` via
   `kubectl patch` (the SwiftGuest controller does this automatically).
3. An init container in the launcher pod runs `qemu-img resize image.raw
   <new-size>` to grow the file to fill the larger PVC.
4. The same init container runs `sgdisk -e` to extend the GPT backup
   header to the new disk end.
5. cloud-init `growpart` extends the partition on first boot, exactly
   as today.

This adds two short init container steps to the snapshot path that the
original optimistic design omitted, but makes the design **portable
across CSI drivers** rather than assuming behavior that only some
drivers exhibit.

For drivers that do support direct larger-size cloning, the
`qemu-img resize` becomes a no-op (file already fills the PVC) and
`sgdisk -e` runs in milliseconds. The init container approach is correct
in both cases.

### What if the requested size is smaller than the snapshot?

This is an error case — you cannot clone into a PVC smaller than the
source. The SwiftGuest controller rejects this at creation time with a
validation error, before creating the PVC. The constraint is documented.

### Same-namespace constraint

The CSI VolumeSnapshot referenced as a clone seed **must live in the
same namespace** as the SwiftGuest creating the clone PVC. This is a
hard constraint, not a recommendation.

Phase 0 spike testing on k0s 1.34 confirmed that the
`CrossNamespaceVolumeDataSource` feature gate is **not** enabled by
default, and when disabled, attempts to use cross-namespace
`dataSourceRef` with a `namespace` field **silently provision an empty
PVC** — no error surfaces, the PVC binds successfully, and the failure
only becomes visible when a VM tries to boot from the empty filesystem.
This is the worst class of failure mode and Phase 1 must avoid it
entirely.

Phase 1 design implications:

- The SwiftImage's clone-seed VolumeSnapshot lives in the SwiftImage's
  namespace.
- All SwiftGuests in that same namespace clone from the same-namespace
  snapshot.
- Operators wanting to share a SwiftImage across namespaces must replicate
  the SwiftImage manifest in each namespace (same operational pattern as
  today — SwiftImage is namespaced).
- The validation webhook rejects any cross-namespace clone-seed reference
  (or, equivalently, the SwiftGuest controller refuses to compose a
  cross-namespace dataSource — the validation can live in either place).
- A future enhancement could lift this when broad cluster support for
  `CrossNamespaceVolumeDataSource` exists and `ReferenceGrant`-style
  authorization is in place. Out of scope for v1.

### Cross-storage-class boundaries

Most CSI drivers can only snapshot/clone within the same storage class.
If the SwiftImage's PVC is on `longhorn` and a SwiftGuest needs its
clone on `nfs`, that's not a snapshot operation — it's a copy. This
design **does not attempt cross-class snapshotting**. The clone PVC's
storage class defaults to the image PVC's storage class. If an
operator overrides via `cloneStorageClassName` on the image and it's
different, behavior depends on whether the CSI driver supports
cross-class snapshot — most don't, in which case the operation fails
with a clear error.

### Validated CSI drivers (from Phase 0 spike + Phase 1 e2e tests)

The following CSI drivers are expected to work with `cloneStrategy:
snapshot`. Phase 0 validated Longhorn directly; the others are based on
documented driver capabilities and will be validated in Phase 1 e2e
tests as cluster availability allows.

| Driver | Clone semantics | Same-size clone | Larger clone | Phase 0 validated |
| --- | --- | --- | --- | --- |
| Longhorn v1.11+ | Full background copy (~10s/GiB) | Yes | Refused — clone-then-expand workaround required | Yes |
| Rook Ceph (RBD) | True CoW | Yes | Yes (expected) | No |
| TopoLVM | LVM thin clone | Yes | Yes (expected) | No |
| AWS EBS CSI | True CoW | Yes | Yes | No (cloud) |
| GCE PD CSI | True CoW | Yes | Yes | No (cloud) |
| Azure Disk CSI | True CoW | Yes | Yes | No (cloud) |
| OpenEBS Mayastor | Snapshot-based | Yes | Driver-dependent | No |

Drivers that **do not** support VolumeSnapshot (and therefore cannot use
this mode):

- `local-path-provisioner` (Rancher's default for k3s)
- `hostpath` provisioner
- NFS without a snapshot-capable CSI overlay

For these, `cloneStrategy: copy` is the only option and remains
supported as the default.

**Driver-specific notes from Phase 0:**

- **Longhorn**: PVC `Bound` state is reached quickly (~3 seconds), but
  pod attach is blocked until the underlying full data copy completes
  (~100 seconds for a 10 GiB image). Pool scaling latency on Longhorn
  is bounded by per-replica clone time. Larger-target clones return
  HTTP 500 from the provisioner; the SwiftGuest controller works around
  this by creating the clone at source size and patching to expand.

- **Cross-namespace `dataSourceRef`**: silently provisions an empty PVC
  on Kubernetes versions where `CrossNamespaceVolumeDataSource` is not
  enabled (k0s 1.34, most stock Kubernetes 1.34 builds). Phase 1
  enforces same-namespace clone seeds — see "Same-namespace constraint"
  above.

### Interaction with SwiftGuestPool scaling

This is the headline benefit. A SwiftGuestPool with `cloneStrategy:
snapshot` on its referenced image scales much faster than the legacy
`copy` path, with the exact gain depending on the CSI driver:

```
Pool scale 5 → 50 replicas (with cloneStrategy: snapshot):
  → 45 new SwiftGuests created
  → each gets a clone PVC via dataSource (driver does the copy)
  → 45 launcher pods scheduled in parallel
  → each VM boots from its own clone
  → growpart extends the partition on first boot

On true CoW drivers (Rook Ceph, EBS, GCE PD):
  Total time bounded by Kubernetes scheduling + cloud-init.
  Per-replica clone is near-instant.

On full-copy drivers (Longhorn):
  Per-replica clone takes ~10 seconds per GiB of image (background copy
  must complete before pod attach). For a 10 GiB image, ~100s per
  replica; replicas can clone in parallel up to the storage cluster's
  bandwidth, so wall-clock for 45 replicas depends on parallelism.
  Still substantially faster than the legacy Copy Job path
  (~3-4× speedup typical), but not "near-instant".
```

The same applies to SwiftGuestPool replacement during rolling updates
and to failed-VM replacement. Pool throughput characteristics under
`cloneStrategy: snapshot` should be measured per-driver in the
operator-facing documentation.

### Interaction with user-facing SwiftRestore (cloneFromSnapshot)

Both mechanisms produce a clone PVC from a CSI VolumeSnapshot. The
difference is the source:

- **SwiftImage clone strategy**: source is the SwiftImage's persistent
  snapshot, lives for the image's lifetime. Operator-invisible.
- **SwiftGuest cloneFromSnapshot**: source is a user-created
  SwiftSnapshot, lives until the operator deletes it. Operator-driven.

The SwiftGuest controller's clone code path is unified: given a
VolumeSnapshot reference (from either source), create a PVC with
`dataSource`. This is the same code, used in both flows.

### Backward compatibility

The existing per-guest cloning architecture (Copy Job at the SwiftGuest
level) is preserved as the `copy` strategy and remains the default for
v1. No existing SwiftImage breaks. No existing SwiftGuestPool breaks.
Operators upgrade their images to `cloneStrategy: snapshot` opt-in.

## CRD Design

Two new CRDs in a new API group `snapshot.kubeswift.io/v1alpha1`:

### SwiftSnapshot

Represents the captured state of a VM at a point in time.

```yaml
apiVersion: snapshot.kubeswift.io/v1alpha1
kind: SwiftSnapshot
metadata:
  name: db-vm-pre-upgrade
  namespace: production
spec:
  guestRef:
    name: database-vm                # SwiftGuest to snapshot
  backend:
    type: csi-volume-snapshot        # csi-volume-snapshot | local | s3
    csiVolumeSnapshot:               # populated when type=csi-volume-snapshot
      volumeSnapshotClassName: csi-snapshotter
    local:                           # populated when type=local
      hostPath: /var/lib/kubeswift/snapshots
    s3:                              # populated when type=s3
      bucket: my-kubeswift-snapshots
      region: us-east-1
      prefix: production/
      credentialsSecretRef:
        name: s3-snapshot-credentials
  includeMemory: true                # ignored for csi-volume-snapshot
  resumeAfterSnapshot: true          # default true; false = leave VM paused
status:
  phase: Pending | Capturing | Uploading | Ready | Failed
  conditions:
    - type: Ready
      status: "True"
  capturedAt: "2026-04-25T10:00:00Z"
  hypervisor: cloud-hypervisor
  hypervisorVersion: "v51.1"
  guestSpec:                          # captured for restore validation
    cpu: 2
    memory: 4096
    imageRef:
      name: ubuntu-noble
  disks:
    - role: root
      sizeBytes: 42949672960
      handle: snap-12345              # CSI snapshot handle, file path, or S3 key
    - role: data
      diskName: data-1
      sizeBytes: 10737418240
      handle: snap-12346
  memorySnapshot:                     # nil when includeMemory: false
    sizeBytes: 4294967296
    handle: snap-mem-12347
  totalSizeBytes: 57982058496
```

### SwiftRestore

Represents the operation of restoring a snapshot into a new (or same) VM.

```yaml
apiVersion: snapshot.kubeswift.io/v1alpha1
kind: SwiftRestore
metadata:
  name: db-vm-restore-after-corruption
  namespace: production
spec:
  snapshotRef:
    name: db-vm-pre-upgrade
  targetGuest:
    name: database-vm                # may match an existing SwiftGuest or new
    overwriteExisting: false         # safety: refuse if target exists unless true
  resumeAfterRestore: true
  identity:                          # for clone use case
    regenerate:
      - hostname
      - machineId
      - sshHostKeys
      - macAddresses
status:
  phase: Pending | Downloading | Restoring | Resuming | Ready | Failed
  conditions:
    - type: Ready
      status: "True"
  guestRef:
    name: database-vm
  startedAt: "2026-04-25T11:00:00Z"
  completedAt: "2026-04-25T11:02:30Z"
```

### SwiftGuest extensions

Add an optional `spec.cloneFromSnapshot` field to support templating:

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: db-clone-1
spec:
  cloneFromSnapshot:
    name: db-vm-pre-upgrade
    identity:
      regenerate:
        - hostname
        - machineId
        - sshHostKeys
        - macAddresses
  # imageRef and kernelRef are ignored when cloneFromSnapshot is set
```

This is sugar over creating a SwiftRestore — under the hood, the SwiftGuest
controller creates a SwiftRestore with the snapshot reference and waits for
it to complete. The benefit is operator ergonomics: "create N clones of
this snapshot" is a Kustomize-friendly pattern.

### Add a `Ready` condition

Both new CRDs must expose a standard `Ready` condition from day one. This
aligns with the GitOps design doc's recommendation and saves a future
migration.

## Identity Regeneration for Cloned VMs

This is the central technical risk in the cloning use case. When a memory
snapshot is restored into multiple VMs, all of them start with identical:

- Hostname
- `/etc/machine-id`
- SSH host keys
- DHCP client identifier
- ARP cache, routing tables
- Process IDs, open file descriptors, etc.

Most of these are recoverable. Some are not.

### Recoverable on first network activity (with cooperation)

- **Hostname**: regenerate via cloud-init "instance-id" change detection,
  or by a guest agent that sets a new hostname on first boot of the clone
- **machine-id**: regenerate by writing a new value to `/etc/machine-id`
  (requires guest cooperation)
- **SSH host keys**: regenerate via systemd unit (`ssh-keygen -A`) or
  cloud-init `ssh_genkeytypes`
- **DHCP**: tied to MAC, regenerated naturally if MAC is changed at the
  hypervisor level on restore

### Hypervisor-level changes

KubeSwift can change the MAC address at restore time by patching the CH
config before restore. This is straightforward — the MAC is in the CH config
JSON that gets restored.

### What about IP?

If the original VM had a DHCP lease for 10.244.125.12, restoring it on the
same network with a different MAC will trigger a new DHCP lease (different
IP). If on a different network, the DHCP server pool determines the IP.
Either way, the IP in the restored guest's network state will be wrong until
the guest's networking stack re-discovers the lease.

The pragmatic solution: **after restore, the controller forces a network
restart inside the guest** via guest agent (if present) or via virtio
network reset. The guest re-DHCPs and gets a valid lease.

### What requires guest cooperation

machine-id regeneration genuinely requires the guest OS to participate.
Without cooperation, two clones will share machine-id, which breaks
applications that key on it (systemd-journald, some monitoring tools).

**Design decision**: KubeSwift will **document the required guest-side
configuration** (a systemd unit, cloud-init module, or guest agent script)
that triggers identity regeneration on first boot of a clone. KubeSwift
itself sets a flag in the snapshot (e.g., a virtio-fs file or a kernel
cmdline parameter on resume) that the guest reads to know "you are a clone,
regenerate yourself."

This is honest about what's possible and where guest involvement is needed.
It mirrors how every VM platform handles this (vSphere customization specs,
Hyper-V VMConnect, OpenStack metadata service).

## Lifecycle Interactions

### With SwiftGuestPool

Pools must work cleanly with snapshots. Two scenarios:

1. **Snapshot a pool member**: same as snapshotting an individual SwiftGuest.
   No special handling needed.
2. **Pool template references a snapshot**: a pool can specify
   `spec.template.spec.cloneFromSnapshot`, and each pool member is created
   as a clone. This is the templating pattern at fleet scale.

### With SwiftGuestClass

The class defines disk size. A snapshot inherits the disk size from the
source VM, not from the class at restore time. If the class has changed
(e.g., disk got bigger), restore uses the snapshot's original size.
Operators who want bigger disks at restore time must manually resize after
restore.

### With root disk cloning

The current per-guest root disk cloning architecture (each SwiftGuest gets
a clone PVC of the SwiftImage) is **directly extended** by this design,
not orthogonal to it. The existing `EnsureRootDiskClone` logic in the
SwiftGuest controller is refactored to produce a clone PVC via either:

- The existing Copy Job path (`cloneStrategy: copy` — default for backward
  compatibility, slow but works on any storage class)
- The new CSI dataSource path (`cloneStrategy: snapshot` — fast CoW clone,
  requires snapshot-capable CSI driver)

The unified clone code path also serves user-facing SwiftRestore: a
restore-from-snapshot operation produces a clone PVC the same way a
SwiftImage-derived clone does. This unification is intentional — there
should be only one code path that turns "a VolumeSnapshot reference"
into "a bound PVC ready for the launcher pod."

The Copy Job pattern is preserved as the fallback for clusters without
snapshot-capable CSI drivers and for backward compatibility with existing
SwiftImages. It is not removed.

### With dataDiskRef / dataDiskRefs

Data disks must be included in the snapshot. The snapshot tracks each disk
by role (`root`, `data`) and per-disk identifier. Restore re-attaches data
disks to the new VM in the same order as the source.

Important: data disks may be shared (referenced by multiple SwiftGuests in
some workflows) or be PVC-templates per-pool-member. Snapshots capture the
state of the disks attached to the VM at the moment of snapshot. The
operator is responsible for understanding sharing semantics — KubeSwift
will not magically split shared disks.

### With multi-NIC and SR-IOV

Multi-NIC VMs: disk-only snapshots work fine. Memory snapshots work for
multi-NIC VMs that use only virtio-net (no SR-IOV VFs). MAC and netdev
state is part of the snapshot.

SR-IOV VMs: memory snapshots are not supported (VFIO constraint). Disk-only
snapshots work. Restore re-allocates a VF on the destination node — the
allocation is fresh, not restored from snapshot.

## What Is Explicitly Out of Scope

Listed here so future work doesn't get confused:

- **Live migration** — separate design doc, separate work plan
- **Incremental snapshots** — only full snapshots in v1; CSI may provide
  incremental at the storage layer transparently
- **Snapshot scheduling / retention policies** — operators run their own
  CronJobs or use Velero/Kasten for policy
- **Cross-cluster snapshot transfer** — Tier C (S3) provides the building
  block, but cross-cluster restore is operator responsibility for v1
- **Crash-consistent vs application-consistent semantics** — KubeSwift
  provides crash-consistent snapshots; application consistency requires
  guest agent cooperation (fsfreeze, database flush) which is a future
  enhancement
- **Snapshot encryption at rest** — relies on the storage backend (CSI
  encryption, S3 SSE)
- **QEMU memory snapshots** — Cloud Hypervisor only for v1; QEMU snapshot
  via QMP is a future addition

## Trade-offs and Open Questions

**Pause window duration**: capture time scales with VM memory size. A 4GB VM
snapshots in seconds; a 1.9TB GPU VM (when memory snapshots become possible
in some future CH version) would take many minutes. The downtime
expectation must be communicated. CSI VolumeSnapshot does not have this
problem because the VM is fully shut down beforehand — explicit downtime
is the contract.

**Snapshot storage growth**: snapshots are large. Operators need to manage
the lifecycle. KubeSwift should expose snapshot size in status and may
emit Prometheus metrics for total snapshot storage usage per namespace.

**Restore conflicts with existing SwiftGuest**: if `targetGuest.name` matches
an existing SwiftGuest, what happens? The design says: refuse unless
`overwriteExisting: true`. Even with the flag, the existing VM is stopped
gracefully before restore overwrites it. Operators who want a side-by-side
clone should target a different name.

**Snapshot of a paused VM**: nothing prevents this, and it produces a valid
paused snapshot. Restore brings it up paused and the resume step takes it
to running. Edge case but worth supporting.

**Open question: what about NFS-backed PVCs?** Many small clusters use NFS
without a CSI snapshot driver. Tier A is unavailable. Tier B works but is
node-local. Tier C works but adds object-storage dependency. The design
accepts this — operators on NFS without snapshots use Tier B for
checkpoints and Tier C for backups.

**Open question: how does this interact with FluxCD GitOps?** SwiftSnapshot
and SwiftRestore are imperative operations triggered by need. Putting them
in Git is awkward — a Git-managed snapshot creates the snapshot on first
apply, then sits there. Recommendation: snapshots are operator-driven
imperative artifacts; restores can be GitOps-managed (declaratively
"this guest should be restored from this snapshot"). The GitOps doc should
get a section on this once snapshots ship.

---

# Part 2 — Phased Work Plan

The work plan delivers value incrementally. Earlier phases must work
correctly before later phases build on them.

## Phase 0 outcomes (completed 2026-04-25)

Phase 0 was completed and the spike results are recorded in
`docs/design/snapshots-spike-results.md`. The spike validated the core
primitives (CH pause/snapshot/resume, CH `--restore`, CSI VolumeSnapshot
with dataSource cloning, byte-level integrity of clones) and uncovered
five findings that have been incorporated into the Concepts section
above:

1. **VFIO failure mode is on restore, not snapshot** — Hard Constraint
   #1 has been corrected. Phase 2 controllers must reject up-front.
2. **Longhorn does full-copy cloning, not CoW** — performance
   expectations and SwiftGuestPool scaling section are now driver-
   aware.
3. **Longhorn refuses larger-target clones** — Phase 1 keeps `qemu-img
   resize + sgdisk -e` in the snapshot path via a `clone-grow-init`
   init container. The fast-path saves the data copy; it does not save
   the disk-grow steps.
4. **Cross-namespace `dataSourceRef` silently fails on k0s 1.34** — the
   "Same-namespace constraint" subsection is now explicit. Validation
   webhook rejects cross-namespace clone-seed references.
5. **Finalizer is load-bearing for true CoW drivers, defensive for
   full-copy drivers** — design keeps the finalizer for cross-driver
   correctness; documentation explains the per-driver nuance.

The Phase 1 scope is unchanged. The implementation effort estimate of
8–12 days remains accurate; the design refinements add minor complexity
(the `clone-grow-init` init container, the same-namespace validation)
but do not change the architectural shape of the work.

A deferred follow-up: source-PVC deletion behavior (what happens when
a SwiftImage's import-artifact PVC is deleted while a snapshot of it
has bound clones) was not tested in Phase 0 for cluster-safety reasons.
This should be validated in a small follow-up spike before Phase 2.

## Phase 0 — Spike and validation (no production code) [COMPLETED]

**Goal:** Prove (a) the Cloud Hypervisor pause/snapshot/restore cycle works
end-to-end manually, and (b) the CSI VolumeSnapshot-based clone path works
on real CSI drivers, before committing to production code.

**Tasks:**

1. On a dev cluster with a working SwiftGuest (Ubuntu Noble, no GPU):
   - Manually `ch-remote pause`, `ch-remote snapshot file:///path`,
     `ch-remote resume`
   - Verify VM keeps running after resume
   - Verify snapshot directory contains expected files
2. Manually restore the snapshot:
   - Spawn a new Cloud Hypervisor with `--restore source_url=file:///path`
   - Verify VM comes up paused
   - Verify resume works
   - Verify network re-converges (DHCP lease, connectivity)
3. Document the actual pause window duration on real hardware for
   different VM sizes (1GB, 4GB, 16GB, 64GB)
4. Test snapshot/restore for an SR-IOV VM and confirm the failure mode is
   what the docs say (informative error, not silent corruption)
5. Test snapshot/restore for a GPU VM and confirm the failure mode
6. **Validate the CSI clone path** on at least two CSI drivers
   (Longhorn and one of: Rook Ceph, TopoLVM, EBS):
   - Create a PVC with a 40GB raw image
   - Create a VolumeSnapshot of it
   - Create a new PVC with `dataSource` pointing at the snapshot
   - Time the operation (target: <30 seconds)
   - Verify the new PVC contains the same image data
   - Boot a Cloud Hypervisor VM from the cloned PVC
7. **Test clone size larger than source**: create a clone PVC at 80GB
   from a 40GB snapshot. Verify:
   - The PVC binds successfully
   - `sgdisk -e` correctly extends the GPT backup header
   - cloud-init `growpart` extends the partition on first boot
   - `df -h` inside the guest shows the larger root filesystem
8. **Test snapshot lifecycle constraints**: with the clone PVC bound and
   in use by a running VM, attempt to delete the source VolumeSnapshot.
   Document each tested driver's behavior — does the deletion succeed,
   block, or leave the clone broken?

**Deliverable:** A short report at `docs/design/snapshots-spike-results.md`
documenting findings, gotchas, performance numbers, per-driver behaviors,
and any deviations from this design doc that need to be reconciled.

**Out of scope:** writing controller code, defining CRDs in Go.

## Phase 1 — Tier A (CSI VolumeSnapshot) + SwiftImage Clone Strategy

**Goal:** Ship disk-only snapshots backed by CSI VolumeSnapshot AND ship
the SwiftImage `cloneStrategy: snapshot` mode that uses the same plumbing.
These two pieces share the CSI VolumeSnapshot integration code and are
much more efficient to build together than separately. The SwiftImage
clone strategy is the operator-visible win that delivers fast pool scaling.

**Tasks:**

1. Define `SwiftSnapshot` and `SwiftRestore` CRDs in
   `api/snapshot/v1alpha1/`. Include only the `csi-volume-snapshot` backend
   type for now; structure the spec so other backends can be added later
   without breaking changes.
2. Add `Ready` condition from the start (per the GitOps design recommendation).
3. Implement `internal/controller/swiftsnapshot/` controller:
   - Watches SwiftSnapshot resources
   - On create: stop the SwiftGuest (graceful shutdown via SIGTERM, fall
     back to delete after 30s — same pattern as existing stop logic)
   - Create VolumeSnapshot resources for root and data PVCs
   - Wait for `readyToUse=true` on each VolumeSnapshot
   - Restart the SwiftGuest if `resumeAfterSnapshot: true` (default)
   - Update SwiftSnapshot status with snapshot handles
4. Implement `internal/controller/swiftrestore/` controller:
   - Watches SwiftRestore resources
   - Validates target — refuse to overwrite unless `overwriteExisting: true`
   - Create new PVCs from VolumeSnapshot sources (`dataSource` field)
   - Create the target SwiftGuest pointing to the new PVCs
   - Update SwiftRestore status as the new guest progresses
5. **SwiftImage clone strategy** (the headline operator-visible feature):
   - Add `spec.cloneStrategy` (`copy` | `snapshot`) to SwiftImage CRD,
     defaulting to `copy` for backward compatibility
   - Add `spec.cloneStorageClassName` and `spec.volumeSnapshotClassName`
   - Add `status.cloneSeed` to SwiftImage status
   - Extend SwiftImage controller: when `cloneStrategy: snapshot`, after
     the import + resize + GRUB patch sequence completes, create a
     VolumeSnapshot of the image PVC and populate `status.cloneSeed`
   - Add finalizer `kubeswift.io/clone-seed-protected` to the
     VolumeSnapshot to prevent deletion while clones reference it
   - SwiftImage deletion: block until all dependent clone PVCs are gone,
     then remove finalizer and allow snapshot GC
6. **Unified SwiftGuest clone path:**
   - Refactor existing `EnsureRootDiskClone` (rootdisk.go) to branch on
     the image's `status.cloneSeed`:
     - if `cloneSeed.kind == VolumeSnapshot`: create PVC with
       `dataSource` referencing the snapshot **at the snapshot's source
       size** (the safe-across-drivers approach — Longhorn refuses
       larger-target clones); then patch the PVC to the SwiftGuestClass
       `rootDisk.size` for expansion (fast path)
     - if `cloneSeed` is nil: existing Copy Job path (legacy)
   - Add a `clone-grow-init` init container to the launcher pod that
     runs (in order) for snapshot-cloned disks:
     - `qemu-img resize image.raw <new-size>` to grow the file to fill
       the expanded PVC
     - `sgdisk -e` to extend the GPT backup header to the new disk end
   - This init container replaces the Copy Job's resize+sgdisk steps
     for the snapshot path. The Copy Job path is unchanged.
   - Validation webhook: reject SwiftGuest creation when requested disk
     size < snapshot size (cannot clone smaller)
   - **Same-namespace enforcement**: validation webhook rejects any
     attempt to create a clone PVC whose dataSource snapshot is in a
     different namespace than the SwiftGuest. This guards against the
     k0s 1.34 silent-failure mode confirmed in Phase 0.
7. RBAC: `snapshot.storage.k8s.io/volumesnapshots` permissions for the
   controller-manager.
8. Helm chart updates: add new CRDs, RBAC.
9. Tests:
   - Unit tests for snapshot/restore controllers
   - Unit tests for SwiftImage clone strategy logic
   - e2e test in `test/snapshot/`: snapshot a SwiftGuest, restore to
     a new name, verify it reaches Running
   - e2e test in `test/clonestrategy/`: create SwiftImage with
     `cloneStrategy: snapshot`, create a SwiftGuestPool with 5 replicas,
     measure scale-up time, verify it's substantially faster than `copy`
     mode (target: <30s per replica vs minutes for copy)
10. `swiftctl snapshot create` and `swiftctl snapshot restore` commands.
11. Documentation:
    - `docs/snapshots/csi-snapshots.md` — user-facing snapshot operator guide
    - `docs/images/clone-strategies.md` — explains `copy` vs `snapshot`,
      validated CSI drivers, when to use each
    - Update `README.md` to mention fast pool scaling with snapshot mode
    - Update SwiftGuestPool docs with scaling performance characteristics

**Deliverable:** Two operator-visible wins from one shared codebase:
(1) operators can snapshot and restore non-memory VM state via
Kubernetes-native CSI VolumeSnapshots, including for GPU and SR-IOV VMs
(disk-only); (2) operators get drastically faster SwiftGuest creation
and pool scaling when their cluster supports CSI snapshots.

**Acceptance:**
- e2e snapshot test passes on a cluster with a snapshot-capable CSI driver
  (Longhorn or Rook Ceph in CI)
- e2e clone strategy test passes: pool scale-up from 1 to 10 replicas
  on Longhorn completes in **substantially less time than `copy` mode**
  (target: at least 3× faster than the same operation with
  `cloneStrategy: copy` on the same cluster — the absolute number
  depends on image size and driver; measure both modes side-by-side
  rather than asserting an absolute threshold)
- Documentation lists validated CSI drivers with their clone semantics
  and per-driver performance characteristics
- `swiftctl snapshot list` shows snapshots with sizes and timestamps
- Backward compatibility: existing SwiftImages with no `cloneStrategy` set
  continue to work via Copy Job path
- SwiftImage deletion correctly blocks while clone PVCs exist
- Cross-namespace clone-seed references are rejected by the validation
  webhook with a clear error message (no silent empty-PVC provisioning)
- Larger-target clone test passes: a SwiftGuestClass with `rootDisk.size`
  larger than the source snapshot produces a working VM with the larger
  filesystem visible inside the guest after first boot

## Phase 2 — Cloud Hypervisor pause/snapshot integration (memory + disk, Tier B)

**Goal:** Add memory snapshots backed by local hostPath storage. This is the
core technical work — wiring up Cloud Hypervisor's API into swiftletd.

**Tasks:**

1. Extend `swift-ch-client` (Rust) with snapshot/restore methods:
   - `pause()` (already used by graceful stop work)
   - `snapshot(destination_url: String)`
   - `restore(source_url: String)`
   - `resume()`
2. Extend swiftletd to expose snapshot operations via a new control surface.
   Options to evaluate in spike:
   - HTTP endpoint inside the launcher pod (controller calls it)
   - Annotation-driven (controller sets annotation, swiftletd watches and
     acts)
   - Sidecar/exec-based (controller execs into the pod)
   The annotation pattern fits KubeSwift's existing reporting model best
   and is the recommended starting point.
3. Extend SwiftSnapshot CRD with the `local` backend type.
4. Extend SwiftSnapshot controller:
   - When backend is `local`, route through swiftletd instead of CSI
   - Pause VM via swiftletd
   - Trigger snapshot capture (swiftletd writes to hostPath)
   - Capture disk artifacts to the same hostPath (cp from PVC mount)
   - Resume VM via swiftletd
   - Record paths in status
5. Extend SwiftRestore controller:
   - When backend is `local`, route through swiftletd
   - Validate the snapshot directory exists on the target node
   - Schedule the new launcher pod with a node selector for the source node
     (Tier B is node-local — restore must happen on the same node)
   - Launcher pod uses `--restore` source URL pointing at the hostPath
   - VM comes up paused, controller resumes it
6. Identity regeneration support:
   - Define the guest-side contract (a virtio-fs marker file or kernel cmdline
     flag indicating "you are a clone, regenerate identity")
   - Document the required guest-side systemd unit and provide a sample
     cloud-init module
   - Implement the controller-side logic that sets the marker on restore
     when SwiftRestore.spec.identity.regenerate is non-empty
7. MAC regeneration: controller patches the CH config in the snapshot
   before restore to assign a new deterministic MAC
8. Tests: e2e snapshot/restore round-trip with memory; clone-from-snapshot
   creating two VMs and verifying machine-id divergence
9. `docs/snapshots/local-snapshots.md` operator guide
10. Document the pause window characteristics (capture time per GB of memory)

**Deliverable:** Operators can capture full VM state (memory + disk) for
non-passthrough VMs to local node storage, and restore it on the same node.
Cloning from snapshot works with identity regeneration.

**Acceptance:**
- e2e test creates a non-GPU VM, modifies in-memory state (writes to a
  tmpfs, runs a process), snapshots, kills the VM, restores, verifies
  in-memory state survived
- Clone test creates two VMs from the same snapshot, verifies machine-id,
  hostname, and SSH host keys differ
- Pause window for a 4GB VM is under 30 seconds on local NVMe
- VFIO VMs receive a clear error when memory snapshot is requested

## Phase 3 — Tier C (S3 export and import)

**Goal:** Make snapshots cluster-portable by adding object-storage backend.

**Tasks:**

1. Extend SwiftSnapshot CRD with the `s3` backend type
2. Extend SwiftSnapshot controller:
   - For `s3`, run Tier B local capture first
   - Then create an Upload Job that pushes to S3 using credentials from
     the referenced Secret
   - Optionally clean up local copy after successful upload
3. Extend SwiftRestore controller:
   - For `s3`-backed snapshots, run a Download Job to pull to local
     hostPath on the target node
   - Then proceed with Tier B's restore path
4. Use a small Go binary (or `aws-cli` / `mc` from MinIO) for upload/download
5. Credentials handling: support standard S3 credential patterns
   (access key + secret, IRSA, IAM role for service accounts)
6. Tests: e2e against a real S3 endpoint (MinIO in CI), including
   cross-node restore
7. `docs/snapshots/s3-snapshots.md` operator guide
8. Document bandwidth expectations and the upload/download dominating total
   operation time

**Deliverable:** Cluster-portable snapshots backed by S3-compatible
object storage.

**Acceptance:**
- e2e test snapshots a VM, deletes the local snapshot, restores from S3
  on a different node
- Performance documented for various VM sizes
- Credential rotation tested

## Phase 4 — Cloning ergonomics and SwiftGuestPool integration

**Goal:** Make the cloning use case first-class through SwiftGuest's
`spec.cloneFromSnapshot` field and SwiftGuestPool support.

**Tasks:**

1. Add `spec.cloneFromSnapshot` to SwiftGuest CRD
2. SwiftGuest controller: when `cloneFromSnapshot` is set, create a
   SwiftRestore under the hood targeting this guest's name; wait for
   `Ready=true` before reporting the guest as Ready
3. Validation: `cloneFromSnapshot` is mutually exclusive with `imageRef`
   and `kernelRef`. Webhook enforces this.
4. SwiftGuestPool: pool template's spec can include `cloneFromSnapshot`.
   Pool members each get their own SwiftRestore from the same snapshot.
5. PVC handling: cloned VMs need fresh per-VM root disk PVCs derived from
   the snapshot's disk handles. Reuse the existing per-guest cloning
   infrastructure with "seed from snapshot" added to the Copy Job's
   capabilities.
6. Documentation: `docs/snapshots/cloning.md` covering golden-image
   workflows, CI scenarios, and pool templating.
7. Tests: pool with 4 members all cloned from a snapshot; verify each
   gets unique identity and IP after first boot

**Deliverable:** First-class VM cloning at both single-VM and fleet scale.

**Acceptance:**
- A pool of 5 VMs created from a snapshot reaches `Ready` in well under
  the time it would take to boot from a SwiftImage (target: 10x faster)
- Each cloned VM has unique machine-id, hostname, and IP
- e2e test in CI

## Phase 5 — Operator polish and observability

**Goal:** Operability surfaces match the rest of KubeSwift's quality bar.

**Tasks:**

1. `swiftctl` commands:
   - `swiftctl snapshot create <guest>`
   - `swiftctl snapshot list`
   - `swiftctl snapshot describe <snapshot>`
   - `swiftctl snapshot delete <snapshot>`
   - `swiftctl restore <snapshot> --target <name>`
2. Prometheus metrics:
   - `kubeswift_snapshot_total{phase, backend}` (gauge)
   - `kubeswift_snapshot_size_bytes{snapshot_name}` (gauge)
   - `kubeswift_snapshot_duration_seconds{operation}` (histogram —
     pause, capture, upload, restore)
   - `kubeswift_snapshot_failures_total{reason, backend}` (counter)
3. Status field improvements: pause window duration, transfer rates
4. Sample Grafana dashboard for snapshot operations
5. Documentation review pass with an operator unfamiliar with the design

**Deliverable:** Snapshot operations are as observable and operable as
the rest of KubeSwift.

## Phase Sequencing and Dependencies

```
Phase 0 (spike) — must come first
    ↓
Phase 1 (CSI / Tier A + SwiftImage clone strategy)
    ↓
Phase 2 (Cloud Hypervisor / Tier B) — depends on Phase 1's CRD scaffolding
    ↓
Phase 3 (S3 / Tier C) — depends on Phase 2's local capture
    ↓
Phase 4 (cloning ergonomics) — depends on Phase 2; can use Tier B or C
    ↓
Phase 5 (polish) — final
```

Phase 1 delivers two pieces of user-visible value from one shared codebase:
disk-only user-facing snapshots (the foundation for the rest of the design)
and the SwiftImage clone strategy that gives operators dramatically faster
VM creation and pool scaling. Building these together is much more
efficient than doing them separately because they share the CSI
VolumeSnapshot integration and the unified clone PVC code path.

Phase 2 adds memory snapshots, which is the headline VM-state-preservation
feature. Phase 3 makes them portable. Phase 4 unlocks the user-facing
cloneFromSnapshot ergonomics at scale. Phase 5 polishes.

## Estimated Effort

Rough sizing for one contributor familiar with KubeSwift:

| Phase | Effort |
|-------|--------|
| 0 — Spike (snapshots + clone strategy) | 3–4 days |
| 1 — CSI snapshots + SwiftImage clone strategy | 8–12 days |
| 2 — CH memory snapshots | 7–10 days |
| 3 — S3 backend | 4–5 days |
| 4 — Cloning ergonomics (cloneFromSnapshot) | 3–5 days |
| 5 — Polish | 2–3 days |

Total: roughly 5–7 weeks of focused work. Phase 1 alone is roughly two
weeks and unblocks two pieces of meaningful operator value: user-facing
snapshots AND fast pool scaling via SwiftImage clone strategy.

## Notes for AI Assistants Picking This Up

1. **Read this whole document first.** The hard constraints in the Concepts
   section are not negotiable and shape the entire design.
2. **Read `kubeswift_context.md`** for current project state. Some details
   in this doc (e.g., specific Cloud Hypervisor versions, root disk
   cloning architecture) may have evolved.
3. **Phase 0 is mandatory.** Do not implement controller code before the
   spike confirms the manual workflow. CH's snapshot API has had bugs in
   the past — verify on the version actually deployed. Validate the CSI
   clone path on real drivers too — driver behavior varies.
4. **VFIO is a hard constraint.** Do not design around it. GPU and SR-IOV
   VMs get Tier A only. Document this everywhere.
5. **Pause window is real downtime.** Tooling, status, docs, and CLI must
   communicate this clearly. Operators should never be surprised that
   snapshot took 20 seconds during which their VM was unresponsive.
6. **Identity regeneration requires guest cooperation.** Do not promise
   automatic identity regeneration without the guest-side contract. The
   controller can set markers; the guest must act on them.
7. **Snapshot opacity.** Snapshot directory contents are CH-internal. Do
   not parse them, do not modify them. Treat as black box.
8. **CRD compatibility.** The `Ready` condition is required from day one.
   Existing patterns (annotations for status reporting, finalizers for
   cleanup, controller-runtime) all apply.
9. **Helm chart sync.** After CRD changes:
   `make generate && cp config/crd/bases/*.yaml charts/kubeswift/crds/`.
10. **Don't couple snapshots to live migration.** Live migration is a
    separate design with overlapping but distinct primitives. This design
    must stand alone and not assume anything about the live migration design.
11. **SwiftImage clone strategy must default to `copy`.** Backward
    compatibility is non-negotiable. Existing images must continue to
    work without changes. The default may flip in a later minor version
    only after broad validation.
12. **Unify the clone PVC code path.** The SwiftImage clone strategy and
    user-facing SwiftRestore both produce a clone PVC from a CSI
    VolumeSnapshot. The SwiftGuest controller's clone logic should be
    written once and consumed by both. Do not duplicate the dataSource
    plumbing across two code paths.
13. **Finalizer correctness.** The `kubeswift.io/clone-seed-protected`
    finalizer on VolumeSnapshots must be removed correctly when the
    SwiftImage is deleted AND no clone PVCs reference it. Wrong logic
    here either leaks snapshots forever or breaks clones. Test deletion
    paths thoroughly.
14. **Storage class compatibility.** Snapshot-based cloning is
    same-storage-class only. Cross-class operations fall back to copy
    or fail loudly — never silently produce a broken clone.
