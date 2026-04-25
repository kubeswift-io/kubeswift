# Environmental issues observed during cleanup — 2026-04-25

> **Purpose:** Diagnose-only report on cluster-environment health observed during the post-Phase-0/Phase-1 cleanup pass. **No remediation was performed** for any item below — recommendations are documented for the operator to evaluate and execute.
> **Cluster:** kubeswift-cluster (k0s 1.34.3) — nodes `boba`, `frida`, `miles`.
> **Companion document:** `cleanup-inventory-2026-04-25.md` (the catalog) and the closing notes of PR #6.

## Summary

| Issue | Severity | Action required |
|---|---|---|
| Boba Longhorn disk `DiskFilesystemChanged` (root cause of all `degraded`/`faulted` volumes) | **High** — every new Longhorn volume created on this cluster is forced into 2-replica `degraded` state instead of the default 3 | Operator must reset the disk record on boba (procedure documented below). |
| Two Longhorn volumes still attached and degraded post-cleanup (`pvc-1665bc7b…`, `pvc-91757340…`) | Medium — they are functional but lack the third replica's redundancy | Will self-heal once boba's disk is back. No manual intervention needed. |
| Two Longhorn volumes detached + `unknown` robustness (`pvc-6bf9f5db…`, `pvc-cb90f2e9…`) | Low — `unknown` is the normal state for idle Longhorn volumes whose engine isn't running | No action — they'll re-attach when the next workload mounts them. |
| boba GPU passthrough state | **None — clean** | Both GPU and audio peer at `0000:01:00.{0,1}` are correctly bound to `vfio-pci`. SwiftGPUNode allocator state is consistent. **Do nothing.** |
| Stuck finalizers post-cleanup | **None** | Step 2 deletion completed without a single finalizer hang. The `kubeswift.io/clone-seed-protected` finalizer (Phase 1) was not exercised because Phase 1 CRDs aren't yet installed on this cluster — note for future cleanup runs. |
| Other RBAC/ServiceAccount drift | **None** | `kubectl get sa,role,rolebinding -n default | grep spike` returned empty. |

---

## 1. Faulted/degraded Longhorn volumes — root cause

### 1.1 Pre-cleanup picture

Before Step 2 deletion ran, `kubectl get volumes.longhorn.io -A` showed:

```
pvc-043d81a2-…  attached  degraded  10G  spike-clone-writer
pvc-1665bc7b-…  attached  degraded  40G  sample
pvc-17f7a332-…  detached  unknown    1G  (spike-xns-source)
pvc-24e6cf27-…  attached  degraded  10G  spike-writer-2
pvc-2ece6b88-…  detached  unknown    1G  (spike-xns-clone)
pvc-470f9654-…  attached  degraded  20G  spike-restore-4g
pvc-5870de39-…  detached  unknown   40G  (spike-clone-then-expand)
pvc-6bf9f5db-…  attached  degraded  40G  ubuntu-noble-qemu import
pvc-91757340-…  detached  faulted   40G  swiftguest-root-qemu-test
pvc-cb90f2e9-…  detached  unknown   10G  (ubuntu-noble import)
```

The `faulted` one is decisive: `kubectl -n longhorn-system get engines.longhorn.io -o json | jq '.items[] | select(.spec.volumeName=="pvc-91757340-…")'` showed `state=stopped, replicas=null`. The volume had **zero healthy replicas anywhere in the cluster** — its 40 GiB engine was started, picked replica targets, all targets failed scheduling, and the volume ended up unrecoverable without operator action.

The four `degraded` volumes have only 2 replicas (default replica count is 3); they're fully serving I/O but lack the third copy's redundancy.

### 1.2 Root cause — boba's Longhorn disk

`kubectl -n longhorn-system get nodes.longhorn.io boba -o yaml | yq .status.diskStatus`:

```yaml
default-disk-3626c798d41cd46e:
  conditions:
  - type: Ready
    status: "False"
    reason: DiskFilesystemChanged
    message: |
      Disk default-disk-3626c798d41cd46e(/var/lib/longhorn/) on node boba is not ready:
      record diskUUID doesn't match the one on the disk
    lastTransitionTime: "2026-04-25T09:57:00Z"
  - type: Schedulable
    status: "False"
    reason: DiskNotReady
  diskUUID: 9dee4de1-95cb-417f-b337-fa22eb7027fe
  diskPath: /var/lib/longhorn/
  filesystemType: ext2/ext3
  storageAvailable: 0
  storageMaximum: 0
  storageScheduled: 0
```

Boba's spec still says the disk is at `/var/lib/longhorn/` and `allowScheduling: true`, but Longhorn refuses to use it because the diskUUID it recorded the first time the disk was registered (`9dee4de1-95cb-417f-b337-fa22eb7027fe`) does **not** match the diskUUID currently present on the filesystem at that path.

Almost certainly cause: the underlying volume backing `/var/lib/longhorn/` on boba was wiped, reformatted, or remounted from a different device after Longhorn first registered the node. This timestamp lines up with `2026-04-25T09:57:00Z` — boba was added to the cluster ~6h50m before the cleanup pass began, the same window in which the spike was running.

Result: every new Longhorn volume (`default-replica-count: {"v1":"3","v2":"3"}`) tries to schedule 3 replicas, finds only 2 of 3 nodes schedulable (frida, miles), and ends up `degraded`. Volumes that happened to attempt replica allocation while frida or miles also had transient pressure (e.g. concurrent spike PVC creation) ended up `faulted`.

### 1.3 Post-cleanup picture

After Step 2 deletion ran (spike PVCs gone, hostpath cleaned up):

```
pvc-1665bc7b-…  attached  degraded  40G  sample
pvc-6bf9f5db-…  detached  unknown   40G  ubuntu-noble-qemu import
pvc-91757340-…  attached  degraded  40G  swiftguest-root-qemu-test  ← was faulted
pvc-cb90f2e9-…  detached  unknown   10G  ubuntu-noble import
```

The previously-`faulted` qemu-test volume (`pvc-91757340-…`) **recovered on its own** to `degraded` once the spike PVCs vacated frida/miles — Longhorn was finally able to schedule 2 of 3 replicas. The `qemu-test` SwiftGuest is now `phase=Running` with `primaryIP=10.244.125.18` (verified post-cleanup).

The two `attached degraded` volumes will continue to operate without the third replica until boba's disk is repaired.

### 1.4 Recommended remediation (for boba's disk only — operator runs)

Per [Longhorn 1.11 docs — "Replacing a Disk"](https://longhorn.io/docs/1.11/nodes-and-volumes/nodes/replace-disk/) and the upstream guidance for `DiskFilesystemChanged`:

1. **Drain workloads** that have replicas on boba (none currently, since it's unschedulable — confirm with `kubectl -n longhorn-system get replicas.longhorn.io -o jsonpath='{range .items[?(@.spec.nodeID=="boba")]}{.metadata.name}{"\n"}{end}'`).
2. **Disable scheduling** for the affected disk on boba via the Longhorn UI or by patching the Node CR:
   ```sh
   kubectl -n longhorn-system patch nodes.longhorn.io boba --type=merge -p \
     '{"spec":{"disks":{"default-disk-3626c798d41cd46e":{"allowScheduling":false,"evictionRequested":true}}}}'
   ```
3. **Remove the disk record** from the Node CR spec (this is the Longhorn-supported way to clear a stale diskUUID — Longhorn will then re-register it with the current diskUUID):
   ```sh
   kubectl -n longhorn-system patch nodes.longhorn.io boba --type=json -p \
     '[{"op":"remove","path":"/spec/disks/default-disk-3626c798d41cd46e"}]'
   ```
4. **Add the disk back** so Longhorn re-registers it:
   ```sh
   kubectl -n longhorn-system patch nodes.longhorn.io boba --type=merge -p \
     '{"spec":{"disks":{"default-disk-boba-fresh":{"path":"/var/lib/longhorn/","allowScheduling":true,"storageReserved":0}}}}'
   ```
5. Verify `kubectl -n longhorn-system get nodes.longhorn.io boba -o jsonpath='{.status.diskStatus}'` reports `Ready=True, Schedulable=True, storageAvailable > 0`.
6. Watch the existing `degraded` volumes self-heal: Longhorn will start scheduling the third replica on boba. `kubectl -n longhorn-system get volumes.longhorn.io --watch` will show the transitions back to `healthy`.

**Caveat — `/var/lib/longhorn/` content:** if there's existing data on `/var/lib/longhorn/` from before boba was first added (replica directories, snapshot blobs), step 3 above will leave them on disk. Longhorn does NOT auto-reclaim them when the disk record is removed. If the operator wants a fully clean re-register, `rm -rf /var/lib/longhorn/replicas /var/lib/longhorn/replicas-deleted` on boba before step 4, AFTER confirming no replicas are still listed for boba in the cluster.

**I cannot tell from observable state alone whether the `/var/lib/longhorn/` content is recoverable replica data or stale leftovers** — there are no replicas currently registered on boba (`kubectl -n longhorn-system get replicas.longhorn.io -A | grep boba` returns empty), but that just means Longhorn isn't *tracking* anything there. Whether the on-disk content is meaningful requires checking the directory listing on the node, which is outside the scope of this report.

### 1.5 Volumes that look recoverable vs. delete-and-recreate candidates

| Volume | Workload | Recoverable? | Recommendation |
|---|---|---|---|
| `pvc-1665bc7b-…` (sample SwiftGuest) | active, 2/3 replicas | Yes — self-heals | Wait for boba disk fix |
| `pvc-91757340-…` (qemu-test) | active, 2/3 replicas | Yes — self-heals (recovered from `faulted` already) | Same |
| `pvc-6bf9f5db-…` (ubuntu-noble-qemu import) | idle | Yes — `unknown` is normal for detached | No action needed |
| `pvc-cb90f2e9-…` (ubuntu-noble import) | idle | Yes | No action needed |

No delete-and-recreate is needed. Every remaining volume is recoverable in place once boba's disk is fixed.

---

## 2. boba GPU passthrough state — clean, no remediation needed

`kubectl -n kubeswift-system exec gpu-discovery-sk4m9 -- lspci -nnk -s 0000:01:`:

```
01:00.0 VGA compatible controller [0300]: NVIDIA Corporation GP104 [GeForce GTX 1080] [10de:1b80] (rev a1)
	Subsystem: NVIDIA Corporation Device [10de:119e]
	Kernel driver in use: vfio-pci

01:00.1 Audio device [0403]: NVIDIA Corporation GP104 High Definition Audio Controller [10de:10f0] (rev a1)
	Subsystem: NVIDIA Corporation Device [10de:119e]
	Kernel driver in use: vfio-pci
```

Both devices in IOMMU group 1 are bound to `vfio-pci`. The SwiftGPUNode controller state agrees:
```
boba.status.gpus[0].driver:    vfio-pci
boba.status.gpus[0].allocated: false
boba.status.gpus[0].allocatedTo: <empty>
```

The spike's `spike-gpu-manual` pod (deleted in Step 2.1) had been holding `0000:01:00.0` continuously since 11:09 UTC. Its `driver_override` writes during the spike's GPU section persisted; on pod teardown, the kernel did NOT rebind to `nvidia` or `snd_hda_intel` because the driver overrides remain set. This is the desired clean state for a node that's a candidate for SwiftGPU passthrough.

No `dmesg` capture is included here because no rebind race occurred during cleanup — the GPU state never changed. (The historical race that bug 34 / Phase 0 spike §4 documented would have manifested if `spike-gpu-manual` had been a SwiftGuest with the `gpu-init` init container; it wasn't, it was a hand-rolled VM pod that wrote `driver_override` directly.)

**Recommendation: do nothing.** If a future operator wants to flip the GPU back to `nvidia` for non-passthrough use, the sequence would be (NOT EXECUTED HERE):

```sh
# On boba, as root:
echo > /sys/bus/pci/devices/0000:01:00.0/driver_override
echo > /sys/bus/pci/devices/0000:01:00.1/driver_override
echo 0000:01:00.0 > /sys/bus/pci/drivers/vfio-pci/unbind
echo 0000:01:00.1 > /sys/bus/pci/drivers/vfio-pci/unbind
modprobe nvidia
modprobe snd_hda_intel
echo 0000:01:00.0 > /sys/bus/pci/drivers_probe
echo 0000:01:00.1 > /sys/bus/pci/drivers_probe
```

---

## 3. Other observations

### 3.1 No stuck finalizers during Step 2

Every deletion in Step 2 completed within seconds. The Terminating `swiftguest-root-spike-vm-4g` PVC cleared automatically once `spike-restore-4g` was deleted (kube-controller-manager's `pvc-protection` finalizer behaving as designed). No finalizer patch was needed.

### 3.2 Phase 1 finalizer (`kubeswift.io/clone-seed-protected`) — not exercised

This is the new finalizer added in PR #6. It is **not yet installed on this cluster** because Phase 1 hasn't merged or deployed — the SwiftSnapshot/SwiftRestore CRDs and SwiftImage `cloneStrategy: snapshot` extensions are absent.

When Phase 1 ships and a future cleanup run encounters SwiftSnapshots or SwiftImages with `cloneStrategy: snapshot`, the cleanup order in the user's task description (SwiftSnapshots/SwiftRestores deleted *before* the SwiftImages they may have seeded) will exercise the finalizer's "no dependent SwiftGuests in this namespace → can remove" branch. That logic was unit-tested in Phase 1 (`TestHandleCloneSeedDeletion_*` suites in `internal/controller/swiftimage/finalizer_test.go`); a real-world test against a degraded Longhorn would still be valuable. Note this for the next cleanup pass, post-Phase-1-deploy.

### 3.3 Two non-default VolumeSnapshotClasses still present

`longhorn-snapshot` (Retain) and `longhorn-snapshot-delete` (Delete). Both flagged AMBIGUOUS-ASK in the inventory. These are NOT environmental issues — they're test fixtures the operator may or may not want to keep for future Phase 1 e2e runs (the Phase 1 e2e scripts auto-detect the default VolumeSnapshotClass but accept `--vsclass`).

### 3.4 No SwiftGuestPool resources

`kubectl get swiftguestpool -A` returned empty. Nothing pool-related to clean up or note.

### 3.5 No spike-related RBAC drift

`kubectl get sa,role,rolebinding -n default | grep -iE "spike"` returned empty. The spike pods used the `default` ServiceAccount and inherited cluster RBAC, leaving no traces.

---

## Recommendation summary for the operator

1. **Fix boba Longhorn disk** (§1.4) — single most important follow-up. Until done, every Longhorn volume on this cluster runs at 2-of-3 replication.
2. **Do nothing about GPU state** (§2) — already clean.
3. **Decide on the AMBIGUOUS items in the inventory** (`sample`/`qemu-test`/`ubuntu-noble-qemu`/`qemu-test-seed` SwiftGuests + their resources, and the two VolumeSnapshotClasses).
4. **After the boba fix**, run `make smoke-test` (full multi-scenario, not just disk-boot) to verify all 5 canonical scenarios pass on a healthy storage layer. This exercises the qemu-boot path that was blocked during the cleanup window.
