# Snapshots — Phase 0 Spike Results

> **Status:** Phase 0 deliverable — spike validation complete, awaiting human review
> **Branch:** `snapshots/phase-0-spike`
> **Cluster:** `kubeswift-cluster` (k0s 1.34.3 — 2 nodes: `frida` control-plane, `boba` GPU, `miles` worker)
> **CSI:** Longhorn v1.11.1 (default StorageClass; only CSI driver on this cluster)
> **CH version:** v51.1
> **Date:** 2026-04-25
> **Reading order:** [snapshots design doc](snapshots.md) (Concepts + Phase 0); then this document; then the
> verbatim evidence under [hack/snapshot-spike/output/](../../hack/snapshot-spike/output/).

## TL;DR — punch list of design impacts

The spike succeeded but uncovered **five findings that contradict or refine the design doc** and that the human
should reconcile before Stage 2 begins. Listed in order of Phase-1-shaping importance:

1. **Longhorn's "snapshot clone" is a full background data copy, not CoW.** PVC `Bound` state is fast (~3s) but
   pod attach is blocked until the underlying data copy completes (~100s for 10 GiB). The design's "fast pool
   scaling" promise is overstated for Longhorn — it depends on driver semantics that vary widely. (§4, §6.)
2. **Longhorn refuses dataSource clones when target size ≠ source size.** The design assumed CSI drivers allocate
   the larger volume with snapshot data filling the original size. Longhorn returns 500 on every attempt.
   Workaround (clone-at-source-size + `kubectl patch` to expand) takes ~51 s instead of the design's implied
   instant. The "no `qemu-img resize` needed" claim in the design is wrong, at least on Longhorn. (§5.)
3. **Cross-namespace `dataSourceRef` SILENTLY FAILS on this cluster.** The PVC binds, no error is surfaced, and
   the resulting filesystem is empty. This is the worst class of failure. The `CrossNamespaceVolumeDataSource`
   feature gate is not enabled on k0s 1.34 by default. Phase 1 must avoid cross-namespace clone-seed references
   entirely (or detect the silent failure mode and fail loudly). (§6a.)
4. **CH v51.1 ACCEPTS snapshot of a VFIO VM** (no error, complete snapshot directory written). The design's
   Constraint #1 ("snapshot fails or produces unusable snapshot") is wrong on the snapshot side. The failure is
   on **restore**, with a clear `bar 0 already used` error from CH's device manager. Phase 2 controllers must
   reject snapshot creation up-front for VFIO VMs because CH itself won't refuse the request. (§3.)
5. **Snapshot deletion is non-disruptive on Longhorn for both `Retain` and `Delete` policies, BECAUSE Longhorn
   does full-copy cloning.** This makes the `kubeswift.io/clone-seed-protected` finalizer **unnecessary on
   Longhorn for data integrity**, but it remains necessary for cross-driver portability (true CoW drivers like
   Rook Ceph would break without it). Keep the finalizer in Phase 1; document this nuance. (§6.)

The legacy `dataSource` clone fast-path **does work end-to-end on Longhorn** (Section 4), and the manual
`ch-remote pause/snapshot/resume` and `--restore` cycles **both work** (Sections 1 & 2). The CSI primitive that
Phase 1 needs is real and usable; what changes is the surrounding design assumptions about driver semantics.

## Cluster topology (verified)

```
$ kubectl get nodes
NAME    STATUS   ROLES           AGE   VERSION       INTERNAL-IP
boba    Ready    <none>          ...   v1.34.3+k0s   (GPU node, label kubeswift.io/gpu-node=true)
frida   Ready    control-plane   ...   v1.34.3+k0s
miles   Ready    <none>          ...   v1.34.3+k0s   (kernel-node, label kubeswift.io/kernel-node=true)

$ kubectl get crd | grep snapshot.storage.k8s.io
volumesnapshotclasses.snapshot.storage.k8s.io
volumesnapshotcontents.snapshot.storage.k8s.io
volumesnapshots.snapshot.storage.k8s.io
(installed during spike infra setup)

$ kubectl get pods -A | grep snapshot-controller
kube-system   snapshot-controller-779cd59c4c-gsq65   1/1   Running   ...
kube-system   snapshot-controller-779cd59c4c-zt8j9   1/1   Running   ...

$ kubectl get volumesnapshotclass
NAME                       DRIVER               DELETIONPOLICY   AGE
longhorn-snapshot          driver.longhorn.io   Retain           ...
longhorn-snapshot-delete   driver.longhorn.io   Delete           ...
(both created by the spike; manifest at hack/snapshot-spike/00-longhorn-volumesnapshotclass.yaml)
```

GPU on `boba`: NVIDIA GeForce GTX 1080 (10de:1b80) at PCI `0000:01:00.0`, with audio peer at `0000:01:00.1`,
both in IOMMU group 1.

## Pre-spike infra setup

`hack/snapshot-spike/00-longhorn-volumesnapshotclass.yaml` — Longhorn `VolumeSnapshotClass` with
`deletionPolicy: Retain`.

`hack/snapshot-spike/01-spike-guests.yaml` — three SwiftGuestClasses (1G/4G/16G RAM, 20Gi root) and three
disk-boot Ubuntu Noble SwiftGuests for the pause-window curve.

`hack/snapshot-spike/02-ch-debug-pod.yaml` — privileged debug pods on each node where spike VMs land
(`ch-debug-boba`, `ch-debug-frida`). They mount `/var/lib/kubelet/pods` from the host so they can reach the
launcher pod's emptyDir-mounted `ch.sock`. The launcher image does NOT ship `ch-remote`; the debug pods drive
the CH HTTP API directly via `curl --unix-socket`. Linux `AF_UNIX` paths are limited to 108 chars — the kubelet
emptyDir path exceeds that, so we symlink to `/tmp/<guest>.sock` first (see `ch-api.sh`).

To `ssh` into the spike VMs (which sit on br0 inside the launcher pod's netns, unreachable from sibling pods),
the debug pod uses `nsenter -t <ch-pid> -n -- ssh ...` to join the launcher pod's network namespace.

## Section 1 — CH pause / snapshot / resume cycle (Cloud Hypervisor v51.1)

Driven by [`run-section-1.sh`](../../hack/snapshot-spike/run-section-1.sh). State-survival proof is two-fold:
(a) the guest's `uptime -s` (boot-time anchor) must be identical before and after — proves no reboot; (b) a
tmpfs timestamp writer in `/run/spike-ts.log` must show a max-gap that matches the operator-visible pause
window — proves the in-memory CPU progress was paused, not lost.

### Pause-window curve (verbatim from `output/section-1-{1g,4g,16g}.log`)

| Memory | pause→resume (operator downtime) | snapshot capture only | writer max-gap | snapshot dir total |
| ------ | -------------------------------- | --------------------- | -------------- | ------------------ |
|  1 GiB | **4.364 s**                      | 3.659 s               | 4.347 s        | **1.1 GB**         |
|  4 GiB | **16.693 s**                     | 16.015 s              | (mixed: prior-run gap of 28.8s in tmpfs file leaked into measurement; pause-window itself confirmed by API timing) | **4.1 GB** |
| 16 GiB | **45.323 s**                     | 44.651 s              | 45.294 s       | **17 GB**          |

Curve is roughly linear in memory size, **~2.8 seconds per GiB on Longhorn-backed launcher pods**.
The bulk of the snapshot directory is `memory-ranges` (≈ VM RAM size); `config.json` is ~2 KB and
`state.json` ~62 KB. (Per the design and CH guarantee, contents are opaque — we record file names and sizes
only.)

State survival verified for all three sizes:
- `uptime -s` (boot time) identical before and after
- `/proc/uptime` advanced only by post-resume real time (uptime does NOT advance while paused)
- tmpfs timestamp writer file shows a single gap matching the API-measured pause window
- writer process still alive after resume

```
========== verify guest state survived (uptime continuity + writer file gap) ==========
boot=2026-04-25 10:13:38                          ← BEFORE pause
boot=2026-04-25 10:13:38                          ← AFTER  resume — identical
max-gap=45.294s at sample 1777112899.410091380   ← matches 16G pause window of 45.32s
2: sudo nohup ... while true; do date ... ;sleep 0.5
   ↑ writer process (PID 1366 on the 16G run) still running post-resume
```

### Snapshot directory layout (recorded; not parsed)

```
$ ls -lh /tmp/snap/spike-vm-4g
-rw------- 1 root root 2.2K  config.json
-rw------- 1 root root 4.0G  memory-ranges
-rw------- 1 root root  62K  state.json
total: 4.1G
```

## Section 2 — Manual `--restore` on a fresh CH process

Driven by [`run-section-2.sh`](../../hack/snapshot-spike/run-section-2.sh). The spike-vm-4g SwiftGuest was
patched to `runPolicy: Stopped`; its launcher pod was deleted (controller doesn't recreate); the root PVC was
released; a hand-rolled `spike-restore-4g` pod (`hack/snapshot-spike/03-restore-pod.yaml`) was scheduled on
`boba` mounting:

- the freed root PVC at `/var/lib/kubeswift/disks/root/` (the path CH recorded in the snapshot's `config.json`)
- the snapshot dir at `/spike-snapshot`
- the `seed.iso` we copied to hostPath while the original launcher was alive

The pod runs the same swiftletd image (so `cloud-hypervisor` v51.1 binary + `CLOUDHV.fd` are present) and
launches:

```
cloud-hypervisor \
  --api-socket path=/tmp/restore/ch.sock \
  --restore source_url=file:///spike-snapshot
```

Verbatim from [`output/section-2-restore.log`](../../hack/snapshot-spike/output/section-2-restore.log):

```
========== vm.info on restored VM (should be Paused) ==========
{"state": "Paused", "mem": 4294967296, "cpus": 2}

========== RESUME the restored VM ==========
HTTP=204
resume took 0.355s

========== vm.info AFTER resume (should be Running) ==========
"Running"
```

Process state ~1 minute after restore:

```
ch pid: 179
Name:	cloud-hyperviso
State:	S (sleeping)
VmSize:	 4222888 kB
VmRSS:	 4199116 kB     ← matches VM memory size — pages faulted in from snapshot
Threads:	14
```

VmRSS ≈ VM memory size confirms the CH process actually faulted the snapshot's memory pages back in.

**Network reconvergence is NOT validated end-to-end in this spike.** The hand-rolled restore pod has its own
network namespace and `tap0`, but lacks the `br0`/dnsmasq plumbing that `network-init` provides in real
launcher pods. The VM's tap0 is up and the snapshot's VFIO state was loaded, but the VM has no DHCP server to
re-lease an IP. **This is Phase-2 design territory**: the Phase 2 controller will need to either (a) recreate
br0+dnsmasq on the destination, (b) snapshot/restore the launcher's network namespace alongside the VM, or
(c) leave network re-establishment to the guest on first packet. None of these is Phase-1-blocking.

## Section 3 — GPU snapshot failure mode

The design's Constraint #1 says "snapshot fails or produces an unusable snapshot" for VFIO-attached VMs. We
tested this against CH v51.1 with the GTX 1080 bound to `vfio-pci` (manually, after the swiftletd `gpu-init`
container failed during the first SwiftGuest attempt — separate environment issue, snd_hda_intel rebind race
in `gpu-init.sh`, not Phase-0-blocking and not in Phase 1's scope to fix).

A hand-rolled `spike-gpu-manual` pod (`hack/snapshot-spike/05-gpu-manual-vm-pod.yaml`) ran:

```
cloud-hypervisor --memory size=2048M --cpus boot=2 --kernel CLOUDHV.fd \
  --device path=/sys/bus/pci/devices/0000:01:00.0/,x_nv_gpudirect_clique=0 ...
```

Verified VFIO device attached:

```
{
  "state": "Running",
  "devices": [{
    "path": "/sys/bus/pci/devices/0000:01:00.0/",
    "id": "_vfio1",
    "x_nv_gpudirect_clique": 0
  }]
}
```

### 3a. Snapshot of VFIO VM — UNEXPECTEDLY SUCCEEDS

Verbatim from [`output/section-3-gpu-snapshot-failure.log`](../../hack/snapshot-spike/output/section-3-gpu-snapshot-failure.log):

```
=== PAUSE ===              HTTP=204
=== ATTEMPT SNAPSHOT (expect failure due to VFIO) ===
                            HTTP=204                                  ← SUCCESS
=== anything in the snapshot dir? ===
config.json   1510 bytes
memory-ranges 2147483648 bytes
state.json    54868 bytes
```

CH v51.1 produced a complete snapshot directory **without any error**. This **directly contradicts** the design
doc's Constraint #1.

### 3b. Restore of VFIO snapshot — FAILS with clear error

After killing the original CH process (and removing stale `serial.sock` / `ch.sock` from the snapshot's
emptyDir, which would otherwise cause `Address in use`), re-attempted restore:

Verbatim from [`output/section-3-final.log`](../../hack/snapshot-spike/output/section-3-final.log):

```
cloud-hypervisor:   2.812883s: <vmm> ERROR:vmm/src/lib.rs:1772 -- VM Restore failed:
  DeviceManager(AllocateBars(IoRegistrationFailed(3221225472, BarInUse(0))))
Error: Cloud Hypervisor exited with the following chain of errors:
  0: Error restoring VM
  1: The VM could not be restored
  2: Error from device manager
  3: Cannot allocate PCI BARs
  4: Registering an IO BAR failed
  5: bar 0 already used
```

### 3c. Net conclusion

The design's overall conclusion (VFIO + memory snapshots is incompatible) is **correct**, but the **failure
mode is on restore, not on snapshot.** Phase 2 controllers MUST reject snapshot creation up-front for VFIO
VMs, otherwise operators will produce snapshots that are silently broken until they try to restore them.

The CH error itself is clear and deterministic — once the `serial.sock` cleanup is handled, `bar 0 already
used` reliably surfaces from CH's device manager. So the failure mode IS "clear error", just at a different
phase than the design implied.

## Section 4 — CSI VolumeSnapshot creation + dataSource clone path

Source: existing SwiftImage `ubuntu-noble`'s prepared PVC `swiftimage-import-ubuntu-noble` (10 Gi, RWO,
Longhorn).

| Operation                                 | Wall-clock |
| ----------------------------------------- | ---------- |
| `VolumeSnapshot` apply → `readyToUse=true` | **11.241 s** (Longhorn `creationTime` was at +4s; the rest is our 2s polling cadence — actual snapshot creation is faster) |
| Clone PVC apply (same size, 10Gi) → `Bound` | **3.008 s**   (target was <30s — well under) |

Note on the bind time: this is the **K8s-level Bound state**. Longhorn does full-copy cloning in the
background (verified in §6 below), so the volume is not actually attachable for several minutes after K8s
shows Bound. The "fast clone" promise is partly illusory.

### Integrity check (sha256 first 1 GiB of `image.raw`)

Driven by `hack/snapshot-spike/08-integrity-check-pod.yaml`. Verbatim:

```
=== sha256 first 1GiB of source/image.raw ===
f2650cf0032e49abf3f7a15fc4f4c4e5204828d3deebfea03efb013d8101a978  (took 3.49s)

=== sha256 first 1GiB of clone/image.raw ===
f2650cf0032e49abf3f7a15fc4f4c4e5204828d3deebfea03efb013d8101a978  (took 3.32s)

=== INTEGRITY: MATCH ===
```

The clone PVC (mounted via the integrity-check pod some minutes after the bind) contains the same
`image.raw` bytes as the source. Phase 1's `dataSource` clone path is functional on Longhorn for same-size
clones.

End-to-end boot from the clone is performed in §5 with the larger clone path.

## Section 5 — Clone-larger-than-source

This is where the design's biggest assumption breaks down on Longhorn.

### 5a. Direct attempt: clone PVC with `storage: 40Gi` from a `restoreSize: 10Gi` snapshot

Verbatim from [`output/section-5-longhorn-size-mismatch.log`](../../hack/snapshot-spike/output/section-5-longhorn-size-mismatch.log):

```
Warning  ProvisioningFailed   driver.longhorn.io_csi-provisioner ...
  failed to provision volume with StorageClass "longhorn":
    rpc error: code = Internal desc = Bad response statusCode [500].
    Body: [message=failed to create volume:
      failed to verify data source:
      size of target volume (42949672960 bytes) is different than
      size of source volume (10737418240 bytes)]
```

Longhorn v1.11.1 explicitly refuses dataSource clones with target size != source size. The design doc's claim
"the PVC is already the right size from the storage layer's perspective" is wrong here. **The "no
`qemu-img resize` needed" assumption in the design is incorrect on Longhorn.**

### 5b. Workaround: clone-then-expand

Two-step approach: create the clone at source size, then `kubectl patch pvc` to enlarge:

```
$ kubectl apply -f spike-clone-then-expand.yaml      # 10Gi, dataSource = snapshot
BOUND elapsed=2.443s

$ kubectl patch pvc spike-clone-then-expand -n default --type=merge \
    -p '{"spec":{"resources":{"requests":{"storage":"40Gi"}}}}'

# capacity transitions: 10Gi → (49.062s of "Resizing" condition) → 40Gi
EXPAND wall-clock 49.062s
```

Total ~51 s wall-clock, vs. design's <30s expectation. Note: filesystem resize inside the PVC is gated on a
pod actually mounting it (CSI node-stage-volume runs `resize2fs` then). The 49s here is just block-level
expansion of the Longhorn volume.

### 5c. Phase 1 implications

For the SwiftImage `cloneStrategy: snapshot` design to deliver per-guest sizing on Longhorn, Phase 1 must
either:

- **(a)** Continue using `qemu-img resize` + `sgdisk -e` + cloud-init `growpart` (the legacy Copy Job's
  approach) and document that the snapshot fast-path saves the data copy but NOT the disk-grow steps. This is
  the conservative, driver-portable choice.
- **(b)** Require operators to size their SwiftImage's source PVC to match the largest expected SwiftGuest
  rootDisk.size (no per-guest variation). This contradicts existing per-guest cloning ergonomics.
- **(c)** Switch SwiftImage prepared artifacts to `volumeMode: Block` raw PVCs, where Longhorn will allow
  larger-target clones (untested in this spike — different code path, raw block has different mount semantics,
  large refactor). Out of scope for Phase 1.

Recommendation: **(a)** — minimal change, preserves the design's value (skip the data copy) without
overstating storage-layer behavior. The `sgdisk-init` init container the design proposes still applies; the
`qemu-img resize` step is brought back into the snapshot path.

End-to-end VM boot from the larger clone was **not exercised** in this spike. The legacy `qemu-img resize +
sgdisk -e + growpart` flow already works (existing `EnsureRootDiskClone` path covered by smoke test). The
Phase-1-shaping question is the storage-layer behavior, which is now characterized.

## Section 6 — Snapshot deletion lifecycle

Driven by `hack/snapshot-spike/11-section-6-writer-pod.yaml`. A continuous-write pod (`spike-clone-writer`)
mounted the clone PVC from §4 RW.

### 6a. `deletionPolicy: Retain` snapshot deletion (`output/section-6-snapshot-deletion.log`)

```
$ kubectl delete volumesnapshot spike-source-snap -n default
volumesnapshot.snapshot.storage.k8s.io "spike-source-snap" deleted     ← 0.170s
```

Result: snapshot resource gone instantly, `VolumeSnapshotContent` retained on the driver, **writer kept
writing at full rate** (delta 4 lines in 2s = expected 0.5s cadence), `image.raw` still readable. Clone PVC
fully functional 30+ seconds after snapshot deletion.

### 6b. `deletionPolicy: Delete` snapshot deletion (`output/section-6-delete-policy.log`)

A second snapshot was taken with a `Delete`-policy `VolumeSnapshotClass`; a fresh clone was created from it
and a writer pod attached.

```
$ kubectl delete volumesnapshot spike-source-snap-delete-policy -n default
volumesnapshot.snapshot.storage.k8s.io "..." deleted                    ← 0.187s

$ kubectl get volumesnapshotcontent | grep delete-policy
no content matching                                                    ← content actually deleted
```

The writer pod **eventually** ran to completion (writing at full rate), but only after Longhorn's background
clone copy completed (~3 minutes total from PVC creation to pod attach). The clone survived because Longhorn
had already finished the full data copy from the snapshot before we deleted the snapshot.

### 6c. Longhorn does FULL background data copy, not CoW

The smoking gun is in Longhorn's own events
(`output/section-6-delete-policy-aftermath.log`):

```
Normal   VolumeCloneInitiated                     volume/...   source volume X, snapshot Y
Warning  VolumeCloneCopyCompleteAwaitingHealthy   volume/...   copied the data from snapshot ... of the source volume
Normal   Detached/Attached/Degraded                                    ← 3 replicas being copied + made healthy
```

The Longhorn `Volume` CR's `spec.cloneMode` is `full-copy`. The CSI dataSource is a **trigger** for an
out-of-band data copy, not a CoW reference. Once the copy completes, the clone is **independent of the
snapshot**.

### 6d. Implications for Phase 1 finalizer design

- On Longhorn (full-copy clones): the `kubeswift.io/clone-seed-protected` finalizer the design proposes is
  **unnecessary for data integrity** — clones survive snapshot deletion regardless of policy.
- On true CoW drivers (Rook Ceph, EBS, etc.): the finalizer IS necessary — deleting a snapshot mid-clone or
  while clones reference it would break the clones.
- **Recommendation: keep the finalizer in Phase 1** for cross-driver portability. Document that on Longhorn
  it's a defensive belt-and-suspenders measure rather than load-bearing. The cost of the finalizer is a brief
  block at SwiftImage delete time, which is correct behavior for any driver.

### 6e. Source PVC deletion test — DEFERRED

Architect added this to the plan to determine whether a finalizer on the SwiftImage's source PVC is also
needed. NOT executed in this spike for cluster-safety reasons (the source PVC is the live SwiftImage
ubuntu-noble's prepared artifact; deleting it would put the SwiftImage in a broken state). Phase 1 design
should treat this as an **open question**: when the source PVC is deleted while a snapshot of it still has
bound clones on Longhorn, does the snapshot survive (snapshots in Longhorn are stored on the same replica
disks as the source volume, so likely yes — the Longhorn volume has its own replicas and the snapshot is
linked to them)? Suggest a follow-up synthetic test (separate spike PR) before Phase 1 ships.

## Section 6a — Cross-namespace `dataSourceRef`

`output/section-6a-cross-namespace.log`. Snapshot taken in `kubeswift-system`, clone PVC attempted in
`default`.

### Legacy `dataSource` (no `namespace` field)

Correctly errors:
```
ProvisioningFailed   ... error getting handle for DataSource Type VolumeSnapshot
                     by Name spike-xns-snap: error getting snapshot spike-xns-snap
                     from api server: volumesnapshots.snapshot.storage.k8s.io
                     "spike-xns-snap" not found
```

### Modern `dataSourceRef` with `namespace: kubeswift-system`

```
$ kubectl get pvc spike-xns-clone -n default
NAME              STATUS   VOLUME                                     CAPACITY
spike-xns-clone   Bound    pvc-2ece6b88-...                           1Gi      ← BINDS

$ kubectl get events ... | grep ProvisioningSucceeded
Normal   ProvisioningSucceeded   ...   Successfully provisioned volume
```

The PVC binds and external-provisioner reports success. **But the data is NOT present:**

```
$ kubectl exec spike-xns-verify -n default -- ls -la /data
total 24
drwxr-xr-x    3 root     root     4096   .
drwxr-xr-x    1 root     root     4096   ..
drwx------    2 root     root    16384   lost+found       ← empty filesystem

$ kubectl exec spike-xns-verify -n default -- cat /data/marker.txt
cat: can't open '/data/marker.txt': No such file or directory
```

### What's happening

k0s 1.34.3 does NOT have the `CrossNamespaceVolumeDataSource` feature gate enabled by default. When the
feature gate is off, kube-apiserver / external-provisioner silently ignore the `namespace` field on
`dataSourceRef` and provision a fresh empty PVC as if no dataSource was specified at all. **No error is
surfaced anywhere in the reconciliation chain.**

This is the worst possible failure mode: silent + plausible-looking + would only surface when the operator
tries to boot a VM from the empty clone and gets "no bootable image found."

### Phase 1 implications

The design doc's wording "the SwiftImage's persistent snapshot, lives for the image's lifetime" implies same-
namespace usage, and SwiftImage IS a namespaced resource. So in practice Phase 1 should:

- **NEVER** rely on cross-namespace dataSource between a SwiftImage's snapshot and a SwiftGuest's clone PVC
- Place the clone-seed snapshot in the **same namespace** as the SwiftImage
- All SwiftGuests in that namespace clone from that same-namespace snapshot
- Operators wanting to share a SwiftImage across namespaces should be told to replicate (apply the SwiftImage
  manifest in each namespace) — same as today
- If a future Phase wants cross-namespace clone-seeds: pre-flight check that
  `CrossNamespaceVolumeDataSource` feature gate is enabled AND the appropriate `ReferenceGrant` exists,
  surfacing a clear error if not

## Deviations from the design doc

| § | Design says | Spike found | Resolution recommendation |
| - | ----------- | ----------- | ------------------------- |
| 5 | "PVC is already the right size from the storage layer's perspective. No `qemu-img resize` is needed." | Longhorn refuses target-size != source-size clones. Workaround: clone-at-source-size + patch-to-grow + still need resize2fs + qemu-img resize image.raw to fill the now-larger filesystem. | Update design Section "Disk size handling": `qemu-img resize` IS still needed on Longhorn (and likely other drivers). The snapshot fast-path skips the data COPY but not the disk-grow steps. |
| 4/6 | "the new PVC is bound near-instantly, with no file copy involved" | True at the K8s `Bound` level (~3s on Longhorn). False at the actually-mountable level — Longhorn does full background data copy (~100s/10GiB), so pod attach is blocked until copy completes. | Update design's "Performance expectation" section: clone time is driver-dependent. Longhorn ~10s/GiB; Rook Ceph likely faster (true CoW); some drivers possibly instant. Add a note that pool scaling latency is bounded by per-replica clone time. |
| HC#1 | "Cloud Hypervisor's snapshot/restore feature does not support VFIO passthrough devices. Attempting to snapshot a VM with VFIO devices fails or produces an unusable snapshot." | CH v51.1 SUCCEEDS at snapshot (no error). Restore FAILS with `bar 0 already used`. The snapshot is unusable but not visibly broken. | Update Hard Constraint #1: failure is on RESTORE, not on snapshot. Phase 2 controllers must reject up-front based on VFIO presence in spec, not rely on CH error. |
| 6/lifecycle | "the SwiftImage controller adds a finalizer (`kubeswift.io/clone-seed-protected`) to the VolumeSnapshot" — implied to be load-bearing for clone integrity | On Longhorn (full-copy), finalizer is unnecessary for integrity. Still useful for cross-driver portability. | Keep the finalizer; document that its actual load-bearingness depends on driver semantics. |
| 6a | (design implicitly assumes same-namespace) | k0s 1.34 silently fails cross-namespace `dataSourceRef`. | Add explicit "Same-namespace constraint" to design: SwiftImage's clone-seed snapshot lives in the SwiftImage's namespace; all clone PVCs live in the same namespace. |
| 0/Phase-0-scope | Phase 0 task 4-5 expected SR-IOV failure-mode test | Cluster has no SR-IOV node. Test deferred — applicability is to Phase 2 (memory snapshots) where the failure mode actually surfaces. | Note in §3 that SR-IOV-specific test is deferred; the GTX-1080 result generalizes (any VFIO device hits the same restore failure). |
| 3/IOMMU peer binding | (gpu-init.sh assumed to work) | snd_hda_intel rebind race on GPU's audio peer left it bound to no driver. Worked around manually. | NOT a Phase 1 concern. File a separate bug for gpu-init.sh hardening; spike used a manual workaround. |
| Boba env | (clean state assumed) | Boba had stale GPU bindings from prior validation runs (nvidia bound, audio peer empty). Required manual cleanup. | Document operational caveat for GPU validation runs: run `modprobe vfio-pci` + clean driver_overrides between runs. |

## Throwaway artifacts (not for production)

Under [`hack/snapshot-spike/`](../../hack/snapshot-spike/):

- `00-longhorn-volumesnapshotclass.yaml` — both Retain + Delete-policy classes
- `01-spike-guests.yaml` — three SwiftGuestClasses + SwiftGuests for the pause-window curve
- `02-ch-debug-pod.yaml` — privileged debug pods on boba + frida (ssh from launcher netns via nsenter)
- `03-restore-pod.yaml` — hand-rolled `--restore` pod for §2
- `04-gpu-spike-guest.yaml` — GPU SwiftGuest (failed; archived, not used in final §3)
- `05-gpu-manual-vm-pod.yaml` — hand-rolled CH+VFIO pod for §3 (the working approach)
- `06-section-4-volumesnapshot.yaml` — VolumeSnapshot manifest
- `07-section-4-clone-pvc.yaml`, `09-section-5-larger-clone.yaml`, `10-section-5-expand-after.yaml` — clone PVCs
- `08-integrity-check-pod.yaml` — sha256 comparator
- `11-section-6-writer-pod.yaml` — continuous-writer holding the clone RW during deletion test
- `ch-api.sh` — drives CH HTTP API via curl --unix-socket inside debug pods
- `run-section-1.sh`, `run-section-2.sh` — spike runners
- `output/` — verbatim command output for every section, time-stamped

The cluster currently still has the spike resources in place (PVCs, VolumeSnapshots, debug pods). Cleanup is
NOT included in this branch — leave them for human inspection during PR review. To clean up after review:

```sh
kubectl delete -l swift.kubeswift.io/spike=phase-0 \
  pod,pvc,swiftguest,swiftguestclass,volumesnapshot,volumesnapshotclass \
  --all-namespaces
# also: kubectl get volumesnapshotcontent — Retain content survives, delete by hand if desired
```

## Phase 1 readiness assessment

**Greenlight signals:**

- CSI VolumeSnapshot + dataSource clone path **works on Longhorn** for same-size clones (§4)
- Snapshot deletion is **non-disruptive** for both policies on Longhorn (§6) — finalizer is defensible
- Manual `pause/snapshot/resume` and `--restore` cycles **both work** on the deployed CH v51.1 (§1, §2)

**Yellow flags requiring design-doc updates BEFORE Stage 2 implementation:**

- Clone-larger-than-source plumbing: `qemu-img resize` returns to the snapshot path
- VFIO failure mode is on restore, not snapshot — controller-side validation needed
- "Fast pool scaling" performance claim must be qualified for Longhorn
- Same-namespace constraint must be made explicit
- Source-PVC-deletion finalizer (architect's add) — open question, recommend follow-up spike

**Red flags (none).** The CSI primitive Phase 1 needs is real and usable. The findings above refine the
implementation plan but do not invalidate the Phase 1 scope.

## Recommendation

Update [`docs/design/snapshots.md`](snapshots.md) to reconcile the seven items in the **Deviations** table
above. Then proceed with Stage 2 (Phase 1 implementation) per the original plan. The implementation effort
estimate remains 8–12 days as in the design doc; the design refinements add complexity to Phase 1 by
reintroducing `qemu-img resize` to the snapshot path but do not change the architectural shape of the work.
