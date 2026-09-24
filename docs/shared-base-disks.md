# Shared-base root disks

Every guest normally gets a private copy of its image: 20 guests of one 3.5 GiB
image cost 20 copies, and each one has to be made before its guest can boot.

`SwiftGuestClass.spec.sharedBaseDisk: true` gives guests of that class a root
disk that is a copy-on-write snapshot of a **base** — one copy of the image, on
the node, shared by every guest of that image there. A guest then costs only
what it writes: measured on a 3.5 GiB Ubuntu image, a second guest's first boot
cost **25 MiB** of pool space against **1.75 GiB** for a copy, and its disk was
ready in 15 s instead of 55 s.

The trade is that the disk is **node-local**. It cannot move, so the guest
cannot either — see [What it refuses](#what-it-refuses).

## Enable it

Label the nodes that may hold a pool. A pool is a large, preallocated file of
that node's own disk, so no node takes one unless it is offered:

```bash
kubectl label node <node> kubeswift.io/basedisk-node=true
```

Then set the field on a class:

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestClass
metadata:
  name: pool-workers
spec:
  cpu: "2"
  memory: 2Gi
  rootDisk:
    size: 10Gi
    format: raw
  sharedBaseDisk: true
```

Guests referencing that class need no changes. The first guest of an image on a
node builds the base (one image copy, once); every guest after it is a snapshot
of it.

`sharedBaseDisk` is **immutable**. Changing it on a class already in use would
move the root disk of every guest on that class at its next restart, discarding
what it had, so create a new class instead.

## What it costs a node

| | |
|---|---|
| Pool size | `swiftGuest.sharedBaseDisk.poolSize` (default `40Gi`), preallocated |
| Where | `/var/lib/kubeswift/thinpool/` on the labelled node |
| When | Created the first time that node builds a shared-base disk |

The size applies to a pool being **created**; raising it later does not grow one
that exists. A node refuses to create a pool that would leave its filesystem
under 10% free — the kubelet's eviction threshold — and says what it has and
what it needs, rather than taking the node into DiskPressure.

## What it refuses

A shared-base disk is a thin device in a node's pool, not a PersistentVolume.
Anything that depends on the storage layer is refused, at admission where
possible, with the reason on the object:

| Operation | Why |
|---|---|
| Migration, in **every** mode | Live migration has no shared storage to move over, and offline migration only repins the guest, which would leave its disk behind. Recreate the guest on the other node. |
| CSI snapshots (`backend.type: csi-volume-snapshot`) | There is no PVC for a VolumeSnapshot to capture. |
| Full-state snapshots (`includeDisk: true`) | The disk export reads a root PVC these guests do not have. |
| `cloneFromSnapshot` | A clone resumes memory captured against its source's disk; a fresh disk built from the image would not match it. |

Memory snapshots still work — `backend.type: local`, or `s3`/`oci` without
`includeDisk`.

This suits fleets of similar, rebuildable guests: pool replicas, CI workers,
short-lived guests. It does not suit a guest whose disk is the thing that
matters.

## Where a guest runs

A guest runs on the node holding its disk, and the controller pins its launcher
there. `spec.nodeName` must agree with that node; if it does not, the guest is
held with the reason rather than started somewhere its disk is not.

Removing the label from a node does not disturb guests already on it. The label
governs where a disk may be **built**.

## Size

A guest's disk is the size it was built at, from the class's `rootDisk.size`.
That size is recorded on the guest (`status.sharedBaseDisk.sizeBytes`) and used
every time the device is mapped, so changing the class later affects only guests
built after the change — raising it for new guests is safe.

## Deleting a guest

Deleting the guest frees its disk. The controller stops the VM, runs a release
Job on the disk's node, and lets the guest go once that succeeds — so a guest
stays in `Terminating` for a few seconds longer than it used to. Deleting the
guest's whole namespace works the same way; the release Job then runs in the
controller's namespace, because a namespace being deleted accepts no new
objects.

If the node is gone from the cluster, its disks went with it and the guest is
released immediately.

## Bases are a cache

A base makes the *next* guest of that image cheap, and nothing depends on it:
the pool reference-counts the blocks a base shares with its guests, so deleting
one leaves every guest derived from it byte-identical and writable.

So bases are kept while there is room and evicted when there is not, least
recently used first — including one whose image has been deleted, which nothing
can use again. The cost of evicting one is the time to write it again for the
next guest of that image.

Bases that guests on the node were snapshotted from go last. Such a base shares
nearly all its blocks with those guests, so deleting it frees almost nothing
while still costing the rebuild. The node records which base each guest came
from (`derived` in the registry, by base device id) when it creates the guest's
disk; guests an earlier version created have no record and are not counted.

## Reading the state

On a labelled node:

```bash
dmsetup status kubeswift-pool          # used/total data and metadata blocks
dmsetup ls --target thin               # one ks-g-<guest-uid> per guest here
cat /var/lib/kubeswift/thinpool/registry.json
```

Pool status reads `<used>/<total>` in 64 KiB blocks. `rw` is healthy;
`out_of_data_space` means the data device is full and writes are queued (60 s
before guests see ENOSPC); `ro` means the metadata device is full and guests are
already taking I/O errors.

On the guest:

```bash
kubectl get swiftguest <name> -o jsonpath='{.status.sharedBaseDisk}'
```

## When a guest is not starting

`StorageReady` carries the reason:

| Reason | Meaning |
|---|---|
| `BaseDiskNoEligibleNode` | No node is labelled `kubeswift.io/basedisk-node=true`. |
| `BaseDiskNodeIneligible` | The node this guest is pinned to is not labelled, or does not exist. |
| `BaseDiskPending` | The disk is being built; the message names the Job and node. |
| `BaseDiskFailed` | The build failed — a full pool, an image larger than the pool, too little room on the node. The message carries the node's own error. Not retried automatically: delete the Job once the cause is fixed. |
| `BaseDiskUnsupported` | The guest uses something a shared-base disk cannot serve (`cloneFromSnapshot`). |
| `BaseDiskReleasing` | The guest is being deleted and its disk is being freed. |
| `BaseDiskReleaseFailed` | The release failed; the message says how to retry, and how to give the disk up instead. |

## Limits

- Node-local: no migration, and a node's loss is its guests' loss. Back up what
  matters from inside the guest, or use a class without `sharedBaseDisk`.
- One pool per node, sized when it is created.
- The number of guests a node can hold is bounded by pool space, not by image
  copies: watch `dmsetup status kubeswift-pool`.
