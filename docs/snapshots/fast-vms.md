# Fast VMs with snapshots and clones

KubeSwift has **two distinct mechanisms** for spinning up VMs faster than a
plain cold boot. They make *different* things fast — pick by what you need.

| Mechanism | What it makes fast | Still cold-boots the OS? |
|---|---|---|
| **A — `cloneStrategy: snapshot`** on a SwiftImage | root-disk **provisioning** | yes |
| **B — `SwiftSnapshot(includeMemory)` + `cloneFromSnapshot`** | the **boot itself** (memory resume) | no — it resumes |

> **Read this first — when "fast clone" is actually faster.** Both mechanisms
> trade on your storage driver. On a **CoW** CSI driver (Ceph RBD, EBS,
> true-CoW Longhorn), a disk clone is near-instant and these are a big win. On a
> **full-copy** driver (default Longhorn), provisioning a clone disk copies the
> whole image — so the "fast" path can be **no faster, or slower**, than a cold
> boot of a quick-booting cloud image. Measured numbers and the honest caveat
> are in [Demo results](#demo-results-tier-b-on-longhorn-full-copy) below.

---

## Mechanism A — `cloneStrategy: snapshot` (fast disk provisioning)

By default each guest's root disk is a **full copy** of the prepared image (a
Copy Job). With `cloneStrategy: snapshot`, the SwiftImage controller takes **one**
CSI VolumeSnapshot of the prepared image (the "clone-seed"); every new guest then
provisions its root PVC as a CSI clone (`dataSource: VolumeSnapshot`) of that
seed — near-instant on a CoW driver.

```yaml
apiVersion: image.kubeswift.io/v1alpha1
kind: SwiftImage
metadata:
  name: ubuntu-fast
  namespace: default
spec:
  format: qcow2
  rootDisk: { size: "10Gi" }
  cloneStrategy: snapshot              # vs the default "copy"
  volumeSnapshotClassName: longhorn-snapshot   # a VolumeSnapshotClass on your cluster
  source:
    http: { url: https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img }
```

The guest **still cold-boots** the OS (cloud-init runs as usual). Only disk
materialization is faster.

**Use it for:** scaling a `SwiftGuestPool` of many identical VMs.

---

## Mechanism B — `cloneFromSnapshot` (fast boot via memory resume)

Boot **one** "golden" VM, configure it (install your app, warm caches), then
capture its **live memory + device state**. Clones boot via Cloud Hypervisor
`--restore` — they **resume the captured RAM byte-for-byte**: no cold boot, no
cloud-init, no app start-up. The VM comes up already at the captured state.

### Step 1 — capture a memory snapshot of a Running, configured guest

```yaml
apiVersion: snapshot.kubeswift.io/v1alpha1
kind: SwiftSnapshot
metadata:
  name: golden-snap
  namespace: default
spec:
  guestRef: { name: golden-src }       # a Running, already-configured source
  backend:
    type: local                         # Tier B — node-local; see Tier note below
    local: { hostPath: /var/lib/kubeswift/snapshots/golden-snap }
  includeMemory: true                   # captures RAM — this is what enables resume
  resumeAfterSnapshot: true             # source keeps running after the capture
```

Capture pauses the source only for a short **pause window** (sub-second for a
small guest), then resumes it.

### Step 2 — boot a guest as an instant clone

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: golden-clone-1
  namespace: default
spec:
  cloneFromSnapshot:
    snapshotRef: { name: golden-snap }
    regenerate: [macAddresses, hostname, machineId, sshHostKeys]
  guestClassRef: { name: default }      # required by the CRD; CPU/mem actually come from the snapshot
```

Or template it on a `SwiftGuestPool` to spin up **N clones from one snapshot**
(see [`config/samples/clone-from-snapshot/`](../../config/samples/clone-from-snapshot/)).

**Tier B (local) vs Tier C (s3).** A Tier B (`type: local`) snapshot lives on the
**capture node**, so its clones run on that same node (`targetNode` is ignored).
For **cross-node** clones or a pool that spreads across nodes, use a Tier C
(`type: s3`) snapshot and set `cloneFromSnapshot.targetNode` per clone — a
node-pinned download Job stages the artifacts onto the target node first. See
[`clone-from-snapshot.md`](clone-from-snapshot.md).

### The identity caveat (fundamental — resume-vs-boot)

A clone **resumes**, it does not boot. So `machine-id`, SSH host keys, hostname,
and the **guest-visible** MAC are inherited from the source. KubeSwift
regenerates the **hypervisor** MAC per clone (bridge-visible, no L2 collision)
and each clone has its own pod network namespace, but in-guest identity collides
until you **reboot each clone once** — cloud-init then re-runs and the
`regenerate:` list fires. For stateless / ephemeral workloads it's fine as-is.
Detail: [`identity-regeneration.md`](identity-regeneration.md).

### Block-root clones are not yet supported

`cloneFromSnapshot` is validated for **Filesystem-root** source guests (the
default `RWO`+`Filesystem` storage). A source on `RWX`+`Block` storage (the
live-migration-capable class) is **not** a supported clone source yet
(Block-root-clone is deferred — snapshot Phase 4 OQ4). Use a Filesystem-root
guest as your golden source.

---

## Demo results (Tier B on Longhorn full-copy)

Measured end-to-end on the dev cluster (3-node k0s, **Longhorn full-copy** storage,
CH v52, stock Ubuntu Noble cloud image, 4Gi guest):

| Step | Time |
|---|---|
| `golden-src` cold boot (disk copy + OS boot + cloud-init) | **50 s** |
| Memory snapshot capture — **pause window** | **645 ms** |
| `golden-clone-1`: disk-copy Job (Longhorn full-copy, 40 GiB) | **54 s** |
| `golden-clone-1`: memory-stage + `--restore` + resume | ~47 s |
| **`golden-clone-1` total** | **101 s** |

**The clone was slower than the cold boot here — and that's expected on this
storage.** Two things drive it:

1. **Full-copy storage.** `cloneFromSnapshot` provisions a *fresh* root disk for
   the clone (the memory snapshot captures RAM + device state, **not** the disk
   contents), and on Longhorn full-copy that disk copy is ~54 s — the dominant
   cost. On a CoW driver it would be near-instant and the clone would come up in
   **single-digit seconds**.
2. **Trivial workload.** Stock Ubuntu cold-boots in ~50 s with nothing to warm
   up, so resuming saves nothing to offset the disk copy.

**When Mechanism B wins:**
- **CoW storage** — the disk clone is near-instant, so resume → seconds.
- **Expensive in-VM warmup** — an app that takes minutes to start (JIT, model
  load, cache warm) is captured *running* in the snapshot; every clone skips all
  of it. A cold boot would repeat it every time.

**What it always gives**, regardless of storage: **state preservation** — the
clone comes up at the exact captured memory state, no cold boot and no cloud-init
re-run.

The mechanism itself is proven on this cluster: `golden-clone-1` reached
`GuestRunning=True` via the restore-receive path (`snapshot-stager` +
CH `--restore`), resuming the captured state; the source's pause window during
capture was only 645 ms.

---

## See also

- [`clone-from-snapshot.md`](clone-from-snapshot.md) — full cloneFromSnapshot
  reference (Tier B/C, pools, cross-node).
- [`local-snapshots.md`](local-snapshots.md) / [`s3-snapshots.md`](s3-snapshots.md)
  — the snapshot backends.
- [`identity-regeneration.md`](identity-regeneration.md) — the resume-vs-boot
  identity model.
- [`../../config/samples/clone-from-snapshot/`](../../config/samples/clone-from-snapshot/),
  [`../../config/samples/local-snapshots/`](../../config/samples/local-snapshots/)
  — runnable manifests.
