# Snapshot Operator Walkthrough

> Audience: KubeSwift operators learning how to use snapshots and
> restores. Each scenario is self-contained — manifests, commands, and
> the output you should see when you run it. Copy-paste-ready.

This walkthrough exercises every snapshot/restore feature shipped
through Phases 0/1/2 of the [snapshots design](../design/snapshots.md):

- **Tier A** — disk-only snapshots backed by CSI VolumeSnapshot
  ([csi-snapshots.md](csi-snapshots.md))
- **Tier B** — memory + disk snapshots stored on a node hostPath
  ([local-snapshots.md](local-snapshots.md))
- **Clone strategies** — fast SwiftImage cloning via VolumeSnapshot
  ([../images/clone-strategies.md](../images/clone-strategies.md))
- **SwiftGuestPool** with snapshot-backed images
  ([../swiftguestpool-guide.md](../swiftguestpool-guide.md))
- **Identity regeneration** — what works, what doesn't, and why
  ([identity-regeneration.md](identity-regeneration.md))

Each scenario uses both `kubectl` (for CRs) and `swiftctl` (for
VM-aware operations like SSH and snapshot inspection). Install
`swiftctl` per [the CLI guide](../cli.md) before starting; everything
else is plain `kubectl`.

The accompanying sample manifests live in
[`config/samples/snapshots-walkthrough/`](../../config/samples/snapshots-walkthrough/).

## Cluster used for this walkthrough

The output captured below comes from a 3-node k0s cluster (`boba`,
`frida`, `miles`) running Longhorn as the default StorageClass with
two `VolumeSnapshotClass`es: `longhorn-snapshot` (Retain) and
`longhorn-snapshot-delete` (Delete). KubeSwift controller-manager is
built from `main`.

If you're following along on your own cluster, your timings, IPs,
and Longhorn-specific details will differ. The numbers in each
scenario are reference points, not specifications.

## Prerequisites

Before any scenario, the cluster needs:

1. KubeSwift CRDs installed (`make deploy` or `kubectl apply -k
   config/crd`).
2. The controller-manager Running with 0 restarts in
   `kubeswift-system`.
3. At least one snapshot-capable CSI driver and a default
   `VolumeSnapshotClass`. Phase 0 spike validated Longhorn; other
   drivers (Rook Ceph, EBS, GCE PD) work if they support
   `VolumeSnapshot` + `dataSource: VolumeSnapshot`.
4. A `SwiftGuestClass` (cluster-scoped). The samples reference
   `default`; apply
   [`config/samples/shared/swiftguestclass-default.yaml`](../../config/samples/shared/swiftguestclass-default.yaml)
   if you don't already have one.

## Setup that bites operators (do this once)

Every scenario applies into a fresh namespace. RBAC must be applied
to that namespace **and** the RoleBinding's subject must be patched
to point at that namespace's default ServiceAccount. Without the
patch, the launcher pod's swiftletd hits `pods is forbidden` errors
and the SwiftGuest never gets a `status.network.primaryIP`:

```bash
NS=snapshots-wt-s1
kubectl create namespace $NS
kubectl apply -k config/rbac -n $NS
# REQUIRED: the rolebinding's subject defaults to namespace=default;
# patch it to your namespace.
kubectl patch rolebinding swiftletd-reporter -n $NS --type=json \
  -p '[{"op":"replace","path":"/subjects/0/namespace","value":"'"$NS"'"}]'
```

This is what `test/smoke/boot-test.sh` does — see
[`apply_rbac()`](../../test/smoke/boot-test.sh) for the same
incantation. ([Why this isn't already in the
docs](walkthrough-findings.md#f2))

---

## Scenario 1 — Disk-only snapshot of a running VM (Tier A)

**Goal.** Take a backup of a running VM's disk, restore it into a
fresh VM with a different name, verify the data made the trip.

This is the simplest disaster-recovery flow. The VM is **not paused**
during snapshot — capture is crash-consistent (equivalent to a hard
reboot at restore time). For application-consistent backups, quiesce
the workload (`fsfreeze`, app-level checkpoint) before snapshotting.

> Your IPs and timings will differ; what matters is that the source
> and restored VMs get *different* IPs from the same
> `10.244.125.0/24` range and that the data on the restored disk
> matches what you wrote on the source.

### Manifests

[`config/samples/snapshots-walkthrough/scenario-1-csi-snapshot/`](../../config/samples/snapshots-walkthrough/scenario-1-csi-snapshot/)

- `01-source.yaml` — SwiftImage (Ubuntu Noble) + SwiftSeedProfile
  (SSH-friendly) + SwiftGuest
- `02-snapshot.yaml` — SwiftSnapshot with `backend.type:
  csi-volume-snapshot`, `volumeSnapshotClassName: longhorn-snapshot`
- `03-restore.yaml` — SwiftRestore with `targetGuest.name: s1-restored`

### Step 1 — Apply the source manifests; wait for Running + IP

```bash
kubectl apply -n snapshots-wt-s1 -f \
  config/samples/snapshots-walkthrough/scenario-1-csi-snapshot/01-source.yaml
```

```
swiftimage.image.kubeswift.io/ubuntu-noble created
swiftseedprofile.seed.kubeswift.io/walkthrough-seed created
swiftguest.swift.kubeswift.io/s1-source created
```

Watch the SwiftImage become Ready and the SwiftGuest reach Running
with an IP:

```
[0s]   swiftimage=Importing  swiftguest=Failed
[95s]  swiftimage=Ready      swiftguest=Failed
[105s] swiftimage=Ready      swiftguest=Scheduling
[189s] swiftimage=Ready      swiftguest=Running
[200s] swiftimage=Ready      swiftguest=Running   ip=10.244.125.17
```

> **Heads up.** The SwiftGuest briefly shows `phase=Failed` while
> the SwiftImage is still importing. This is cosmetic — once the
> image is Ready, the guest moves through `Scheduling → Running`
> normally. ([Why](walkthrough-findings.md#f4))

On this cluster the SwiftImage import took ~95 s and the SwiftGuest
reached Running with an IP at ~200 s.

### Step 2 — SSH in and write a sentinel

The seed profile in `01-source.yaml` provisions a `kubeswift` user
authorised by the test environment's `~/.ssh/id_ed25519.pub`.
Replace the public key with your own for production use.

```bash
swiftctl ssh s1-source -n snapshots-wt-s1 -- \
  bash -c 'echo "hello-from-source-vm" \
    | sudo tee /var/local/scenario1.txt; sync'
```

For comparison after the restore, capture the source's identity:

```
SENTINEL: hello-from-source-vm
MID:      0a99e6b5a270474388a899a62caf4e9e
HOSTNAME: scenario-1-source
```

### Step 3 — Take the snapshot

```bash
kubectl apply -n snapshots-wt-s1 -f \
  config/samples/snapshots-walkthrough/scenario-1-csi-snapshot/02-snapshot.yaml
```

Watching the SwiftSnapshot progress:
```
[0s] phase=Capturing
[7s] phase=Ready
```

On this cluster the snapshot was Ready in ~7 seconds — Longhorn's
VolumeSnapshot is metadata + a copy-on-write reference, not a full
data copy.

```bash
swiftctl snapshot describe s1-disk-snap -n snapshots-wt-s1
```

```
Name:        s1-disk-snap
Namespace:   snapshots-wt-s1
Guest:       s1-source
Backend:     csi-volume-snapshot
VSClass:     longhorn-snapshot
Phase:       Ready
CapturedAt:  2026-04-28 10:58:09Z
Hypervisor:  cloud-hypervisor
Image:       ubuntu-noble
Disk:        role=root size=42949672960 handle=snapshots-wt-s1/swift-snap-s1-disk-snap
Conditions:
  Ready=True reason=SnapshotReady message="VolumeSnapshot is readyToUse"
```

> **Polish gap.** `swiftctl snapshot describe` shows `size` in raw
> bytes (`42949672960` = 40 GiB). The Tier B Pause Window (relevant
> in Scenario 5) also doesn't surface here. ([F5](walkthrough-findings.md#f5))

The SwiftSnapshot's status field `disks[0].handle` references an
underlying Kubernetes `VolumeSnapshot` object. Operators can inspect
it directly:

```bash
kubectl get volumesnapshot -n snapshots-wt-s1
```

```
NAME                       READYTOUSE   SOURCEPVC                    SOURCESNAPSHOTCONTENT   RESTORESIZE   SNAPSHOTCLASS       SNAPSHOTCONTENT                                    CREATIONTIME   AGE
swift-snap-s1-disk-snap    true         swiftguest-root-s1-source                            40Gi          longhorn-snapshot   snapcontent-...                                    7s             10s
```

This is the storage-system-level handle KubeSwift wraps. If your
CSI driver complains about a snapshot, this is the object to
inspect with `kubectl describe`.

### Step 4 — Restore into a new SwiftGuest

```bash
kubectl apply -n snapshots-wt-s1 -f \
  config/samples/snapshots-walkthrough/scenario-1-csi-snapshot/03-restore.yaml
```

Watching the SwiftRestore + the new SwiftGuest:
```
[0s]   restore=Restoring  guest=          ip=
[6s]   restore=Resuming   guest=Scheduling ip=
[83s]  restore=Resuming   guest=Running   ip=
[88s]  restore=Ready      guest=Running   ip=
[105s] restore=Ready      guest=Running   ip=10.244.125.15
```

On this cluster the restore reached `Ready` in ~88 s, of which most
was **Longhorn cloning the per-guest PVC from the VolumeSnapshot** —
the restored SwiftGuest can't be scheduled until its PVC is `Bound`
at the target size. On copy-on-write-capable drivers (Rook Ceph
RBD, EBS, GCE PD) this phase is near-instantaneous; on Longhorn
(full-copy) it scales roughly linearly with disk size.

> **Doc gap.** [csi-snapshots.md](csi-snapshots.md) doesn't yet
> mention the per-guest PVC clone wait, so operators can think
> "stuck in Restoring" is broken. ([Storage class
> compatibility](../images/clone-strategies.md#storage-class-compatibility-matrix))

### Step 5 — Verify the data made the trip

The restored SwiftGuest is at IP `10.244.125.15` (different from the
source's `10.244.125.17` — the clone runs in its own pod with its
own dnsmasq and gets a fresh DHCP lease). SSH in and check the
sentinel:

```bash
swiftctl ssh s1-restored -n snapshots-wt-s1 -- \
  bash -c 'cat /var/local/scenario1.txt; cat /etc/machine-id; hostname'
```

```
SENTINEL: hello-from-source-vm                    (matches source — disk restored)
MID:      0a99e6b5a270474388a899a62caf4e9e         (same as source — disk restored)
HOSTNAME: scenario-1-source                        (same as source — disk restored)
```

The sentinel survived. The machine-id and hostname are identical
because the restored disk is a byte-for-byte copy of the source's
PVC at snapshot time; for a clone with regenerated identity see
Scenario 6 below.

### How to tell a real restore from a fresh boot

The contract that makes the disk restore actually work: the
per-guest PVC for the restored SwiftGuest must carry
`dataSource: VolumeSnapshot` and the
`swift.kubeswift.io/restore-seeded: "true"` label. Without those,
the SwiftGuest controller would create a fresh PVC seeded from the
SwiftImage instead of from the snapshot, and the "restore" would
silently produce a fresh boot.

```bash
kubectl get pvc swiftguest-root-s1-restored -n snapshots-wt-s1 -o yaml
```

What you should see:
```yaml
spec:
  dataSource:
    apiGroup: snapshot.storage.k8s.io
    kind: VolumeSnapshot
    name: swift-snap-s1-disk-snap
metadata:
  labels:
    swift.kubeswift.io/restore-seeded: "true"
  ownerReferences:
  - kind: SwiftRestore
    name: s1-disk-restore
```

And **no Copy Job** for the restored guest:
```bash
$ kubectl get job -n snapshots-wt-s1 | grep rootclone-s1-restored
$    # empty — restore-seeded path correctly skipped the Copy Job
```

If any of those three signals is wrong — no `dataSource`, missing
`restore-seeded` label, or a Copy Job present for the restored
guest — the SwiftGuest controller has fallen back to seeding a
fresh disk from the SwiftImage instead of from the snapshot, and
the "restore" is producing a fresh boot. See
[walkthrough-findings.md F1](walkthrough-findings.md#f1) for the
failure mode and how it was caught.

### What you just did

You captured a running VM's disk and brought it back into a new VM
with the same data — the simplest disaster-recovery primitive
KubeSwift offers. Same pattern applies whether you're snapshotting
a database, a build cache, or a development VM.

### Cleanup

```bash
kubectl delete -n snapshots-wt-s1 -f \
  config/samples/snapshots-walkthrough/scenario-1-csi-snapshot/03-restore.yaml \
  config/samples/snapshots-walkthrough/scenario-1-csi-snapshot/02-snapshot.yaml \
  config/samples/snapshots-walkthrough/scenario-1-csi-snapshot/01-source.yaml
kubectl delete namespace snapshots-wt-s1
```

The `kubeswift.io/clone-seed-protected` finalizer on SwiftSnapshots
created via `swiftctl snapshot create` is a no-op for Tier A
walkthrough snapshots (no SwiftImage references the snapshot as a
clone seed); deletion proceeds cleanly.

### What's next

Scenario 2 takes a different angle on the same VolumeSnapshot
machinery: instead of taking a snapshot of a running VM, it uses
the snapshot strategy on the **SwiftImage itself**, so every
SwiftGuest booted from the image gets a fast PVC clone via
`dataSource` rather than a per-guest Copy Job. Useful when you scale
fleets from a base image.

---

## Scenario 2 — `cloneStrategy: snapshot` for a single SwiftGuest

**Goal.** Take the same source URL and use it to build two SwiftImages
— one with the default `cloneStrategy: copy`, one with
`cloneStrategy: snapshot` — then boot a SwiftGuest from each and see
how the per-guest PVC provisioning differs. The snapshot strategy is
the prerequisite for fast pool scaling (Scenario 3) and is its own
operator decision when sizing a fleet.

The two strategies produce identical-bit-for-bit guest disks; the
difference is the *path* the per-guest PVC takes from "I want a
copy of the SwiftImage" to "Bound at the target size, ready to
boot."

### Manifests

[`config/samples/snapshots-walkthrough/scenario-2-fast-clone-single/`](../../config/samples/snapshots-walkthrough/scenario-2-fast-clone-single/)

- `01-images.yaml` — two SwiftImages from the same Ubuntu Noble URL:
  `ubuntu-noble-copy` (default) and `ubuntu-noble-fast`
  (`cloneStrategy: snapshot`, `volumeSnapshotClassName:
  longhorn-snapshot`).
- `02-seed-and-guests.yaml` — one SwiftGuest from each image, applied
  in parallel for comparison.

### Step 1 — Apply both SwiftImages; observe the extra phase

```bash
kubectl apply -n snapshots-wt-s2 -f \
  config/samples/snapshots-walkthrough/scenario-2-fast-clone-single/01-images.yaml
```

Watching both images in parallel:
```
[0s]   copy=Importing  fast=Importing
[85s]  copy=Importing  fast=Validating
[169s] copy=Validating fast=Validating
[190s] copy=Ready      fast=Snapshotting   <- extra phase
[200s] copy=Ready      fast=Ready          cloneSeed=ubuntu-noble-fast-clone-seed
```

The snapshot-strategy image goes through an extra `Snapshotting`
phase: after the prepared raw image lands in the SwiftImage's PVC,
the controller takes a deterministic clone-seed VolumeSnapshot named
`<image>-clone-seed` and waits for `readyToUse=true` before flipping
the SwiftImage to `Ready`. The handle is exposed in
`status.cloneSeed`:

```bash
kubectl get swiftimage ubuntu-noble-fast -n snapshots-wt-s2 \
  -o jsonpath='{.status.cloneSeed}{"\n"}'
```

```
{"apiGroup":"snapshot.storage.k8s.io","kind":"VolumeSnapshot","name":"ubuntu-noble-fast-clone-seed"}
```

That's the VolumeSnapshot every guest booted from this image will
use as its `dataSource`. The seed lives as long as any SwiftGuest
references the image (the
`kubeswift.io/clone-seed-protected` finalizer keeps it pinned).

### Step 2 — Boot a SwiftGuest from each image; compare

```bash
kubectl apply -n snapshots-wt-s2 -f \
  config/samples/snapshots-walkthrough/scenario-2-fast-clone-single/02-seed-and-guests.yaml
```

Two SwiftGuests apply at the same time:
- `s2-copy` — boots from `ubuntu-noble-copy` (Copy Job path)
- `s2-snapshot` — boots from `ubuntu-noble-fast` (dataSource clone path)

Watching both in parallel:
```
[0s]   copy=Scheduling                            snapshot=Scheduling
[91s]  copy=Running                               snapshot=Scheduling
[113s] copy=Running ip=10.244.125.19              snapshot=Scheduling
[125s] copy=Running ip=10.244.125.19              snapshot=Running
[147s] copy=Running ip=10.244.125.19              snapshot=Running ip=10.244.125.15
```

On this cluster the copy strategy reached Running with an IP at
**113 s**; the snapshot strategy at **147 s** — 34 seconds slower.

> **Counterintuitive result.** [`clone-strategies.md`](../images/clone-strategies.md)
> implies the snapshot strategy is faster on snapshot-capable
> drivers. At **single-guest scale on Longhorn with a significant
> resize delta** (10 GiB SwiftImage → 40 GiB SwiftGuestClass), the
> opposite is true. ([F7](walkthrough-findings.md#f7))

### Step 3 — Inspect what each path actually did

```bash
kubectl get pvc swiftguest-root-s2-copy -n snapshots-wt-s2 -o yaml
kubectl get pvc swiftguest-root-s2-snapshot -n snapshots-wt-s2 -o yaml
```

The two PVCs ended up at the same size (40 GiB) but got there
through different paths:

| Path | dataSource | Job ran? | Init containers on launcher pod |
|---|---|---|---|
| `cloneStrategy: copy` (s2-copy) | none | `swiftguest-rootclone-s2-copy` (74 s) | network-init only |
| `cloneStrategy: snapshot` (s2-snapshot) | `VolumeSnapshot/ubuntu-noble-fast-clone-seed` | (no Copy Job) | clone-grow-init, network-init |

The snapshot path's `clone-grow-init` runs `qemu-img resize` +
`sgdisk -e` against the cloned PVC after Longhorn finishes expanding
it from the 10 GiB source size to the 40 GiB target size. That
expand-and-wait, plus the init container, is what makes single-guest
slower than the Copy Job path on Longhorn.

### Why pick snapshot strategy then?

Two reasons, both fleet-scale:

1. **Pool scaling.** Five concurrent Copy Jobs serialize on the
   SwiftImage's PVC (one reader at a time on most CSI drivers).
   Five concurrent `dataSource: VolumeSnapshot` clones don't —
   each is independent. Scenario 3 measures this.
2. **Copy-on-write storage.** On Rook Ceph RBD, AWS EBS, or GCE
   Persistent Disk, the `dataSource` clone is near-instantaneous —
   no full data copy. The expand-and-wait phase is also fast
   because the underlying technology grows the volume metadata
   without copying. The snapshot strategy's overhead on
   single-guest collapses on these drivers.

If your storage is full-copy (Longhorn, NFS-style) and you only
ever boot one guest from each image, **stick with the default copy
strategy**. If you scale fleets, or you're on a CoW driver, the
snapshot strategy's setup cost amortises.

### What you just did

You produced two functionally identical SwiftImages with different
internal provisioning paths and saw the trade-off operators face
when picking between them. The choice is the storage-class question
("is my CSI copy-on-write?") and the workload-shape question ("am
I scaling a fleet or booting singletons?").

### Cleanup

```bash
kubectl delete -n snapshots-wt-s2 -f \
  config/samples/snapshots-walkthrough/scenario-2-fast-clone-single/02-seed-and-guests.yaml \
  config/samples/snapshots-walkthrough/scenario-2-fast-clone-single/01-images.yaml
kubectl delete namespace snapshots-wt-s2
```

The `ubuntu-noble-fast-clone-seed` VolumeSnapshot is owned by the
`ubuntu-noble-fast` SwiftImage and is GC'd when the image is
deleted. If you've already used the seed for clone Pools (Scenario 3
or your own workloads), the `kubeswift.io/clone-seed-protected`
finalizer blocks deletion until no SwiftGuests reference the image.

### What's next

Scenario 3 takes the snapshot-backed image from this scenario and
scales a SwiftGuestPool against it. That's where the snapshot
strategy's pool-scale advantage shows up — same provisioning cost
per replica as you saw here, but it parallelises in a way the Copy
Job path does not.

---

(Scenarios 3–8 follow.)
