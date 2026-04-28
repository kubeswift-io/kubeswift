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

## Scenario 3 — SwiftGuestPool scaling on a snapshot-backed image

**Goal.** Take the snapshot-backed image pattern from Scenario 2 and
put it under the load it was designed for: a SwiftGuestPool that
scales up several replicas at once, each cloning the same
clone-seed VolumeSnapshot in parallel.

### Manifests

[`config/samples/snapshots-walkthrough/scenario-3-pool-scaling/`](../../config/samples/snapshots-walkthrough/scenario-3-pool-scaling/)

- `01-image.yaml` — snapshot-backed `ubuntu-noble-pool` SwiftImage +
  shared seed profile.
- `02-pool.yaml` — SwiftGuestPool `pool-fast` at `replicas: 5`,
  template references the snapshot-backed image.

### Step 1 — Create the pool at 5 replicas

```bash
kubectl apply -n snapshots-wt-s3 -f \
  config/samples/snapshots-walkthrough/scenario-3-pool-scaling/01-image.yaml
# wait for SwiftImage Ready (~100s on this cluster)
kubectl apply -n snapshots-wt-s3 -f \
  config/samples/snapshots-walkthrough/scenario-3-pool-scaling/02-pool.yaml
```

Watching `running_with_ip` as a count of pool members reporting
`status.network.primaryIP`:

```
[0s]   running_with_ip=0/5
[228s] running_with_ip=1/5
[269s] running_with_ip=2/5
[279s] running_with_ip=3/5
[310s] running_with_ip=4/5
[321s] running_with_ip=5/5
```

On this cluster the pool reached all 5 replicas Ready in ~321 s.
Replicas come up in waves rather than strictly serially — Longhorn
clones happen concurrently from the shared clone-seed, but
replication-completion timing varies per replica.

### Step 2 — Scale up to 10

```bash
kubectl scale swiftguestpool pool-fast -n snapshots-wt-s3 --replicas=10
```

Five new replicas come up incrementally. On this cluster the **10th
replica got stuck Pending** because the cluster ran out of
schedulable CPU:

```
0/3 nodes are available: 3 Insufficient cpu. no new claims to
deallocate, preemption: 0/3 nodes are available: 3 No preemption
victims found for incoming pod.
```

Each SwiftGuestClass `default` requests 2 vCPU; 9 running replicas
× 2 = 18 vCPU committed, plus controller-manager + Longhorn
instance-managers + system workloads, fills 24 vCPU across three
8-CPU nodes. ([F8](walkthrough-findings.md#f8))

> **Operator finding.** When sizing pools, account for system
> overhead per node. On this cluster, three 8-CPU nodes can host
> ≈9 default-class pool replicas before scheduler pressure kicks
> in. Reserve headroom or scale node count, not just replica count.

### Step 3 — Scale down to 3

```bash
kubectl scale swiftguestpool pool-fast -n snapshots-wt-s3 --replicas=3
```

The pool controller deletes highest-index replicas first. Within
~1 s the API reports replicas=3; the surviving members are
`pool-fast-0`, `pool-fast-1`, `pool-fast-2`. Their launcher pods
continue serving traffic uninterrupted; only pool members 3–9
terminate.

### Step 4 — Verify per-replica state isolation

Each surviving pool member has its own per-guest PVC, all cloned
from the same `ubuntu-noble-pool-clone-seed` VolumeSnapshot:

```
pool-fast-0: dataSource=VolumeSnapshot/ubuntu-noble-pool-clone-seed, ip=10.244.125.12
pool-fast-1: dataSource=VolumeSnapshot/ubuntu-noble-pool-clone-seed, ip=10.244.125.10
pool-fast-2: dataSource=VolumeSnapshot/ubuntu-noble-pool-clone-seed, ip=10.244.125.20
```

Writing a unique sentinel into each replica's `/var/local/who.txt`
confirms independent disks — each replica reads back its own
content; no cross-talk.

### What you just did

You scaled a SwiftGuestPool from 0 → 5 → 10 → 3 replicas using the
snapshot-strategy clone path. You saw the cluster's CPU ceiling
when fleet size exceeded available scheduling capacity, and you
confirmed each pool member has fully independent per-VM disk and
network state despite sharing a clone seed.

### Cleanup

Pool deletion cascades to all SwiftGuests via owner references:

```bash
kubectl delete namespace snapshots-wt-s3
```

### What's next

Scenario 4 stays on the same pool but updates its template — a
**rolling update** that cycles each replica through the controller's
maxUnavailable/maxSurge gates. The snapshot strategy keeps each
replica's PVC clone fast.

---

## Scenario 4 — Pool rolling update

**Goal.** Update a SwiftGuestPool's template (changing the seed
profile) and watch the controller cycle each replica through the
update without dropping below `maxUnavailable`.

### Manifests

[`config/samples/snapshots-walkthrough/scenario-4-pool-rolling-update/`](../../config/samples/snapshots-walkthrough/scenario-4-pool-rolling-update/)

- `01-new-seed.yaml` — `walkthrough-seed-v2`, identical to the
  Scenario 3 seed except for the hostname, which becomes the
  visible signal that the rolling update completed.
- `02-pool-updated.yaml` — same `pool-fast` resource, template's
  `seedProfileRef` now points at `walkthrough-seed-v2`.

### Step 1 — Apply the v2 seed and the updated pool template

```bash
kubectl apply -n snapshots-wt-s3 -f \
  config/samples/snapshots-walkthrough/scenario-4-pool-rolling-update/01-new-seed.yaml
kubectl apply -n snapshots-wt-s3 -f \
  config/samples/snapshots-walkthrough/scenario-4-pool-rolling-update/02-pool-updated.yaml
```

```
swiftseedprofile.seed.kubeswift.io/walkthrough-seed-v2 created
swiftguestpool.swift.kubeswift.io/pool-fast configured
```

The `template.spec` hash changes when `seedProfileRef` does;
`pool.status.templateHash` flips, the rolling update fires.

### Step 2 — Watch the rolling update execute

```
[0s]   replicas=3 ready=1 updated=2
[142s] replicas=3 ready=2 updated=2
[173s] replicas=3 ready=2 updated=3
[298s] replicas=3 ready=3 updated=3   <- complete
```

On this cluster the rolling update completed in ~298 s — each
replica taking ~99 s on average to be cycled. The default
`maxUnavailable: 1` keeps the pool with at least 2/3 ready
throughout.

### Step 3 — Verify v2 hostname applied to all replicas

```bash
for g in pool-fast-0 pool-fast-1 pool-fast-2; do
  swiftctl ssh $g -n snapshots-wt-s3 -- hostname
done
```

```
pool-fast-0: scenario-4-pool-v2
pool-fast-1: scenario-4-pool-v2
pool-fast-2: scenario-4-pool-v2
```

All three pool members rebooted with the new seed profile;
cloud-init applied the v2 hostname on each fresh boot. Pool members
got fresh DHCP leases (different IPs from before the update) — VMs
cycle cleanly through the update.

### What you just did

You updated a pool's template, watched the controller cycle each
replica through a rolling update, and verified the new template's
seed profile applied to every member. The snapshot-strategy clone
path means each cycled replica's PVC gets a fast `dataSource`
clone — no Copy Job overhead per cycle.

### Cleanup

Same as Scenario 3: `kubectl delete namespace snapshots-wt-s3`.

### What's next

Scenario 5 switches to the other backend — Tier B local-hostPath
memory snapshots — and demonstrates the in-place restore flow that
preserves in-RAM state across a launcher pod kill.

---

## Scenario 5 — Memory snapshot + in-place restore (Tier B disaster recovery)

**Goal.** Capture a running VM's full state — RAM included —
to a node hostPath, kill the launcher pod (simulating node failure
or pod eviction), and bring the VM back **with its in-memory state
intact**. The contract Tier B exists to support.

### Manifests

[`config/samples/snapshots-walkthrough/scenario-5-memory-snapshot-inplace/`](../../config/samples/snapshots-walkthrough/scenario-5-memory-snapshot-inplace/)

- `01-source.yaml` — SwiftImage + SwiftSeedProfile + SwiftGuest
  (same shape as Scenario 1).
- `02-snapshot.yaml` — SwiftSnapshot with `backend.type: local`,
  `includeMemory: true`, hostPath under
  `/var/lib/kubeswift/snapshots/`.
- `03-inplace-restore.yaml` — SwiftRestore with `targetGuest.name:
  s5-source` (same as source) and `overwriteExisting: true`.

### Step 1 — Source up; plant a sentinel on tmpfs

The whole point of memory snapshots is preserving state that lives
**only in RAM**. tmpfs is the cleanest test: anything on tmpfs is
gone the moment the kernel restarts. If a sentinel on tmpfs
survives the kill+restore cycle, the captured RAM image was loaded
correctly.

```bash
swiftctl ssh s5-source -n snapshots-wt-s5 -- \
  bash -c 'echo "hello-from-tmpfs" | sudo tee /run/sentinel.txt; \
           stat -f -c %T /run'
```

```
hello-from-tmpfs
tmpfs    <- confirms /run is RAM-only
```

### Step 2 — Take the memory snapshot

```bash
kubectl apply -n snapshots-wt-s5 -f \
  config/samples/snapshots-walkthrough/scenario-5-memory-snapshot-inplace/02-snapshot.yaml
```

The snapshot pauses the VM, serialises RAM to disk, and resumes:

```
[0s]  phase=Capturing
[17s] phase=Ready  pauseWindow=10153ms
```

On this cluster a 4 GiB VM took ~10 seconds of pause time — the VM
is unresponsive on the network during the pause. See
[`pause-window.md`](pause-window.md) for the per-storage-class
slope you'll see; Longhorn HDD measured here is ~2.5 s/GiB.

### Step 3 — Kill the launcher pod (simulating a node failure)

```bash
kubectl delete pod -l swift.kubeswift.io/guest=s5-source \
  -n snapshots-wt-s5 --grace-period=0 --force
```

The SwiftGuest controller will normally requeue and recreate the
launcher pod from the source SwiftImage on its own. To force the
restore-from-snapshot path instead, apply the SwiftRestore
immediately:

### Step 4 — Apply the in-place SwiftRestore

```bash
kubectl apply -n snapshots-wt-s5 -f \
  config/samples/snapshots-walkthrough/scenario-5-memory-snapshot-inplace/03-inplace-restore.yaml
```

```
[1s]  restore=Resuming guest_ip=10.244.125.11
[11s] restore=Ready
```

On this cluster the in-place restore completed in ~11 s — far
faster than a fresh boot because the **fast path** skips the
snapshot-stager init container: when target name == source name
and no identity regeneration is requested, the launcher pod mounts
the snapshot directory read-only and reads it directly, no
`cp -r` of the snapshot bytes.

### Step 5 — Verify the tmpfs sentinel survived

```bash
swiftctl ssh s5-source -n snapshots-wt-s5 -- cat /run/sentinel.txt
```

```
hello-from-tmpfs
```

The sentinel survived because the captured RAM image was loaded —
not a fresh boot. tmpfs lives in RAM; if the kernel had rebooted,
`/run` would be empty.

`uptime` shows minutes since the resume rather than minutes since
the original cold boot — that's an artifact of CH's
restore-resumes-the-clock behavior, not a sign that the kernel
rebooted.

### What you just did

You captured a running VM's full state (disk + memory), simulated
a launcher pod crash, and brought the VM back with in-RAM state
intact. This is the disaster-recovery contract Tier B exists for —
fast restore of a known-good moment, no application restart, no
re-init of in-memory caches.

### Cleanup

```bash
kubectl delete namespace snapshots-wt-s5
```

The `kubeswift.io/snapshot-hostpath-cleanup` finalizer triggers an
on-node cleanup pod that removes the snapshot directory before the
SwiftSnapshot is GC'd.

### What's next

Scenario 6 keeps the same memory snapshot but restores it under a
**different** target name — the clone path. That's where the
documented identity-collision behaviour shows up.

---

## Scenario 6 — Memory snapshot clone restore (with identity collision evidence)

**Goal.** Restore a memory snapshot into a target with a different
name and observe what KubeSwift can and cannot rewrite at clone
time. The identity-collision behaviour is documented in
[`identity-regeneration.md`](identity-regeneration.md); this scenario
captures it empirically so operators can verify on their own
cluster.

### Manifests

[`config/samples/snapshots-walkthrough/scenario-6-memory-snapshot-clone/`](../../config/samples/snapshots-walkthrough/scenario-6-memory-snapshot-clone/)

- `01-clone.yaml` — SwiftRestore against the snapshot from
  Scenario 5 with `targetGuest.name: s6-clone-a` and the full
  `identity.regenerate` set.

### Step 1 — Apply the clone restore

```bash
kubectl apply -n snapshots-wt-s5 -f \
  config/samples/snapshots-walkthrough/scenario-6-memory-snapshot-clone/01-clone.yaml
```

```
[0s]   restore=Restoring  guest=Scheduling
[110s] restore=Resuming   guest=Running
[116s] restore=Ready
```

On this cluster the clone restore reached `Ready` in ~116 s. Most
of that is the snapshot-stager init container: it copies the
snapshot directory into the launcher pod's emptyDir, then patches
`config.json` (rewrite source's runtime_dir prefix in disk paths
+ serial socket, swap MAC for a deterministic clone-specific value,
null `host_mac` so CH auto-discovers the new tap MAC, append the
`kubeswift.clone=true` cmdline marker).

### Step 2 — Compare source and clone identity

The clone shares the source's IP because the kernel network stack's
DHCP lease was captured in RAM. From within either launcher pod,
both VMs are reachable at `10.244.125.11`. SSH to each from inside
its own launcher pod (each pod has its own bridge in its own
network namespace, so the apparent IP is different than what the
source would see).

```bash
# Identity from source
swiftctl ssh s5-source -n snapshots-wt-s5 -- bash -c \
  'echo MID=$(cat /etc/machine-id); hostname; \
   ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub | awk "{print \$2}"; \
   ip -br link show ens3'
```

```
MID=e0b5cf1b07f7490a9ade0bb79763083f
scenario-5-source
SHA256:y8NWi/pIL0+d6K5GA3tOdVkxBG2geGDbd2Eqi5ZL5Ng
ens3 UP 2e:87:2b:07:50:ae <BROADCAST,MULTICAST,UP,LOWER_UP>
```

```bash
# Same query against the clone
swiftctl ssh s6-clone-a -n snapshots-wt-s5 -- bash -c '...'
```

```
MID=e0b5cf1b07f7490a9ade0bb79763083f       <- same as source
scenario-5-source                          <- same as source
SHA256:y8NWi/pIL0+d6K5GA3tOdVkxBG2geGDbd2Eqi5ZL5Ng    <- same as source
ens3 UP 2e:87:2b:07:50:ae ...              <- same as source
```

All four guest-observable identity signals **match the source**.
This is the documented behavior — see
[`identity-regeneration.md`](identity-regeneration.md) for why.

### Step 3 — Observe the hypervisor-side rewrite (which doesn't reach the guest)

```bash
kubectl exec -n snapshots-wt-s5 s6-clone-a -c launcher -- \
  curl -s --unix-socket \
  /var/lib/kubeswift/run/snapshots-wt-s5-s6-clone-a/ch.sock \
  http://localhost/api/v1/vm.info \
  | jq '.config.net[0]'
```

```json
{
  "tap": "tap0",
  "mac": "52:54:00:4d:e2:76",          <- per-clone deterministic MAC
  "host_mac": "1a:2a:53:7f:55:f5",     <- auto-discovered from clone tap
  ...
}

cmdline: "kubeswift.clone=true"        <- patcher installed marker
```

The CH-side `config.net[0].mac` is **different** for the clone
(`52:54:00:4d:e2:76`) than for the source. The bridge fdb on the
clone's launcher pod sees this rewritten MAC. **But the guest-side
view of the same NIC is unchanged** — the virtio-net driver's MAC
is cached in the snapshot's RAM image and survives `--restore`
verbatim.

### Why does the rewrite not propagate?

Because resume is not a boot. CH `--restore` resumes the captured
guest state byte-for-byte: kernel does not re-init, systemd does
not re-init, cloud-init does not re-run, and the virtio-net driver
keeps its cached MAC. The cmdline marker is installed in the CH
config, but the bootcmd that's supposed to grep `/proc/cmdline`
for it never executes because there's no boot.

L2 collisions between source and clone running on the same node
are avoided **only because each launcher pod runs in its own
Kubernetes pod network namespace** — different bridges, different
ARP tables, no cross-pod L2 path to collide on. If you exposed
clone traffic to a shared L2 segment outside the pod network,
both VMs would advertise the source's MAC.

### What operators can do today

For genuinely independent identity, choose one:

1. **Reboot the clone after first resume** — `swiftctl ssh
   s6-clone-a -- sudo reboot`. The fresh boot runs cloud-init,
   regenerates machine-id / SSH keys / hostname normally.
2. **Run regen commands manually inside the clone** — see
   [`identity-regeneration.md`](identity-regeneration.md) "What
   operators have today" for the script.

The vsock-agent path that closes this gap without a reboot is a
future phase.

### What you just did

You confirmed the documented Phase 2 behaviour: hypervisor-side
MAC and cmdline marker land in CH config correctly, but the
guest-observable identity (machine-id, hostname, SSH host keys,
guest-side MAC) collides with the source. Pod network namespace
isolation is what makes the collision operationally tolerable.

### Cleanup

`kubectl delete namespace snapshots-wt-s5`.

### What's next

Scenario 7 explores combining SwiftGuestPool with memory snapshots
— the cloning ergonomics that Phase 4 will close. Today, the
combination has gaps; the scenario documents what works and what
doesn't.

---

## Scenario 7 — SwiftGuestPool templated from a memory snapshot (Phase 4 gap)

**Goal.** Document what's available today for "spin up N VMs all
cloned from one memory snapshot" and where the gaps are. **No
manifests for this scenario** — what's missing is the API surface,
not a working flow.

### What works today

- **Single-clone restore** from a memory snapshot — exercised in
  Scenario 6.
- **Pool scaling on a snapshot-backed SwiftImage** — exercised in
  Scenario 3. The clone seed is a `VolumeSnapshot` of the
  SwiftImage's PVC, not a SwiftSnapshot of a running VM.

### What's missing — Phase 4 design

Per [`docs/design/snapshots.md`](../design/snapshots.md) §"Phase 4
— Cloning ergonomics", these API surfaces are deferred:

1. **`spec.cloneFromSnapshot` on SwiftGuest.** A SwiftGuest spec
   would reference a SwiftSnapshot (memory snapshot) as the boot
   source. Today this field doesn't exist on the CRD; SwiftGuest
   has only `imageRef`, `kernelRef`, `gpuProfileRef`.
2. **Pool template referencing `cloneFromSnapshot`.** With the
   SwiftGuest field above, the pool template's
   `spec.template.spec` could carry it and each pool member would
   become a clone of the snapshot.

Verifying the gap:

```bash
kubectl explain swiftguest.spec | grep -i clone
```

```
   imageRef        <Object>
   kernelRef       <Object>
   gpuProfileRef   <Object>
   guestClassRef   <Object>
   ...                                          <- no cloneFromSnapshot
```

### Adjacent paths that don't substitute

- **`SwiftImage.cloneStrategy: snapshot`** clones a SwiftImage's
  template PVC. It does not clone a SwiftSnapshot (which captures
  a running VM's full state).
- **Manually replicating SwiftRestore objects.** You could write
  N SwiftRestores against the same SwiftSnapshot with different
  `targetGuest.name` values. Each restore would produce a
  full-state clone of the snapshot. But:
  1. The pool controller doesn't manage these — you maintain N CRs
     by hand or via your own tooling.
  2. Identity collision (Scenario 6) hits N times — every clone
     shares the source's machine-id, hostname, etc. until rebooted
     or manually regenerated.

### What this scenario produces

No commands, no output. The finding: **fast pool spin-up from a
memory snapshot is not a Phase 0/1/2 capability**. Operators
needing it today should build pools on a `cloneStrategy:
snapshot` SwiftImage (Scenario 3); a memory-snapshot-templated pool
is Phase 4.

### What's next

Scenario 8 audits the validation webhooks: which inputs the API
rejects up front, with what error messages.

---

## Scenario 8 — Failure modes and operator diagnosability

**Goal.** Verify the validation webhooks reject obviously broken
inputs with operator-comprehensible error messages — the operator
"can I tell what went wrong?" audit.

### Manifests

[`config/samples/snapshots-walkthrough/scenario-8-failure-modes/`](../../config/samples/snapshots-walkthrough/scenario-8-failure-modes/)

Each test below applies a deliberately broken manifest with
`--dry-run=server` so the apply is rejected by the admission
webhook without persisting state.

### Test A — Tier B hostPath outside the allowed prefix

The hostPath whitelist exists so a malicious or mistaken manifest
can't write into arbitrary node directories.

```bash
kubectl apply --dry-run=server -n snapshots-wt-s8 -f - <<EOF
apiVersion: snapshot.kubeswift.io/v1alpha1
kind: SwiftSnapshot
metadata: {name: bad-hostpath}
spec:
  guestRef: {name: irrelevant}
  backend:
    type: local
    local: {hostPath: /tmp/badprefix/foo}
  includeMemory: true
EOF
```

```
Error from server (Forbidden): admission webhook
"vswiftsnapshot.snapshot.kubeswift.io" denied the request:
spec.backend.local.hostPath must be under
/var/lib/kubeswift/snapshots/ (got "/tmp/badprefix/foo")
```

Operator-respecting: names the constraint, names the offending
value.

### Test B — Restore target conflict (controller-level, not webhook)

A SwiftRestore targeting an existing SwiftGuest without
`overwriteExisting: true` **passes the webhook** but fails at the
controller level once it tries to materialise the target:

```
kubectl get swiftrestore conflicting-restore -o yaml
```
```yaml
status:
  phase: Failed
  conditions:
  - type: Ready
    reason: TargetConflict
    message: 'SwiftGuest <name> already exists; set
      targetGuest.overwriteExisting=true to replace'
```

The feedback loop is one reconcile pass slower than webhook
rejection but the message is correct and actionable.
([F9](walkthrough-findings.md#f9))

### Test C — Memory clone without macAddresses regen

A memory clone without MAC regen would put two VMs on the same L2
segment with the same MAC; the webhook prevents it:

```
Error from server (Forbidden): admission webhook
"vswiftrestore.snapshot.kubeswift.io" denied the request:
cloning memory snapshot s5-mem-snap into a different target
(<name> != source <source>) requires spec.identity.regenerate
to include macAddresses; without MAC regeneration the clone
shares network identity with the source and will conflict on
the same L2 segment
```

This is exemplary error-message authoring: WHAT (regen list
incomplete), WHY (L2 collision), and the fix (add macAddresses).

### Test D — `cloneStrategy: snapshot` without volumeSnapshotClassName

```
Error from server (Forbidden): admission webhook
"vswiftimage.image.kubeswift.io" denied the request:
spec.volumeSnapshotClassName is required when
spec.cloneStrategy is 'snapshot'
```

### Test E — VFIO/GPU SwiftGuest + includeMemory: true

VFIO + memory snapshot is the Phase 0 spike Constraint #1 case —
permanently rejected.

```
Error from server (Forbidden): admission webhook
"vswiftsnapshot.snapshot.kubeswift.io" denied the request:
includeMemory=true is not supported when source SwiftGuest
<name> has gpuProfileRef (VFIO + memory snapshot fails on
restore with 'bar 0 already used' — Phase 0 Constraint #1)
```

This message cites the design source for operators who want to
understand the constraint. Operator-respecting at the highest tier.

### Test F — SwiftGuest with both imageRef and kernelRef

The webhook rejects it cleanly:

```
Error from server (Forbidden): admission webhook
"vswiftguest.swift.kubeswift.io" denied the request:
exactly one of spec.imageRef or spec.kernelRef must be set
```

### What you just did

You verified the validation webhooks reject six classes of broken
input with messages an operator can act on. Five of the six are
webhook-level (immediate); one is controller-level (one reconcile
pass slower) and is documented as an acceptable trade-off rather
than a bug.

### Cleanup

These tests use `--dry-run=server` and persist nothing; no cleanup
needed.

---

## After the walkthrough — where to read next

- Per-scenario findings (bugs, doc gaps, UX issues, design gaps)
  surfaced during this exercise: [walkthrough-findings.md](walkthrough-findings.md)
- The CSI snapshot operator guide: [csi-snapshots.md](csi-snapshots.md)
- The Tier B memory snapshot operator guide: [local-snapshots.md](local-snapshots.md)
- The identity regeneration design and resume-vs-boot constraint:
  [identity-regeneration.md](identity-regeneration.md)
- The pause window cost model: [pause-window.md](pause-window.md)
- SwiftImage clone strategies: [../images/clone-strategies.md](../images/clone-strategies.md)
- SwiftGuestPool ops guide: [../swiftguestpool-guide.md](../swiftguestpool-guide.md)
