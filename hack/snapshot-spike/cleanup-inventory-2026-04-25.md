# Cleanup inventory — 2026-04-25

> **Status:** Step 1 only. Read-only. No deletions performed.
> **Cluster:** kubeswift-cluster (k0s 1.34.3, nodes: boba, frida, miles)
> **Source:** Phase 0 spike + Phase 1 e2e test artifacts + smoke-test leftovers
> **Authorize before Step 2:** every item categorized **DELETE** below requires human sign-off before deletion proceeds.

## Summary table

| Category | DELETE | PRESERVE | AMBIGUOUS — ASK |
|---|---|---|---|
| SwiftGuests | 1 (`spike-vm-4g`) | 1 (`faas-test`) | 2 (`sample`, `qemu-test`) |
| SwiftGuestClasses | 4 (`spike-1g`/`-4g`/`-16g`/`-gpu`) | 1 (`default`) | 0 |
| SwiftImages | 1 (`ubuntu-noble-qemu`) | 1 (`ubuntu-noble`) | 0 |
| SwiftKernels | 0 | 1 (`faas-minimal`) | 0 |
| SwiftSeedProfiles | 1 (`qemu-test-seed`) | 1 (`minimal`) | 0 |
| SwiftSnapshots / SwiftRestores | n/a — CRDs not yet installed | n/a | n/a |
| SwiftGPUNode | 1 (`mock-gpu-node`) | 1 (`boba`) | 0 |
| SwiftGPUProfile | 0 | 1 (`gtx1080-single`) | 0 |
| Pods (default ns) | 7 spike-labelled + 1 stuck rootclone | 1 launcher (`faas-test`) | 1 (`sample`) — see below |
| PVCs (default + kubeswift-system) | 5 spike + 1 stuck Terminating | 2 SwiftImage import | 2 (rootclone PVCs of ambiguous guests) |
| VolumeSnapshots | 1 (`spike-xns-snap` in kubeswift-system) | 0 | 0 |
| VolumeSnapshotContents | 2 orphaned Retain-policy contents | 0 | 0 |
| VolumeSnapshotClasses | **see decision below** | 0 | 2 (`longhorn-snapshot`, `longhorn-snapshot-delete`) |
| ConfigMaps (default ns) | 2 (`qemu-test-*`) | 2 (`faas-test-*`) | 2 (`sample-*`) |
| HostPath remnants | `/var/lib/kubeswift/spike-snapshots/` on boba + frida | n/a | n/a |
| Stuck finalizers | 1 PVC | n/a | n/a |
| Longhorn unhealthy volumes | **diagnose-only** — see environmental report | n/a | n/a |
| boba GPU passthrough | **diagnose-only — STATE IS CLEAN** | vfio-pci on both 0000:01:00.{0,1} | 0 |

---

## How to read this inventory

- **DELETE** — labelled `swift.kubeswift.io/spike=phase-0` OR named after spike scripts OR named after a smoke-test scenario that already ran. Safe to remove on operator approval.
- **PRESERVE** — production fixtures (the cluster's working `default` SwiftGuestClass, the `ubuntu-noble` image used as the canonical disk-boot source, the `faas-minimal` SwiftKernel, the `boba` SwiftGPUNode populated by the gpu-discovery DaemonSet, the `gtx1080-single` SwiftGPUProfile from Tier 1 GPU validation work).
- **AMBIGUOUS — ASK** — created by the `make smoke-test` run that finished a few minutes ago. Whether to delete depends on whether the operator wants the cluster fully clean or wants the smoke-test fixtures preserved for the next run.

---

## 1. KubeSwift custom resources

### 1.1 SwiftGuests

**Command:** `kubectl get swiftguest -A -o wide`

```
NAMESPACE   NAME        AGE
default     faas-test   7h41m
default     qemu-test   17m
default     sample      27m
```

| Name | Verdict | Reason |
|---|---|---|
| `faas-test` | **PRESERVE** | Long-lived kernel-boot test (12d) from production validation; not from the spike or Phase 1. |
| `sample` | **AMBIGUOUS — ASK** | Created 27 min ago by `make smoke-test` (disk-boot scenario, ran to PASS). Smoke test was started with `--no-cleanup` so the resource is still here. Deleting is safe — operator decides. |
| `qemu-test` | **AMBIGUOUS — ASK** | Same provenance as `sample`. The qemu-boot scenario failed because the underlying clone PVC's Longhorn volume went `faulted` — see §6. Deleting `qemu-test` will release the stuck clone Job pod (`swiftguest-rootclone-qemu-test-scrwm`), which is currently in `ContainerCreating` waiting on the faulted volume. |

**Spike SwiftGuests already deleted by hand earlier today** (per session notes): `spike-vm-1g`, `spike-vm-4g`, `spike-vm-16g`, `spike-gpu-vm`. Their per-guest root-disk PVC `swiftguest-root-spike-vm-4g` is still here in `Terminating` (see §3).

### 1.2 SwiftGuestClasses (cluster-scoped)

**Command:** `kubectl get swiftguestclass`

```
NAME        AGE
default     12d
spike-16g   6h37m
spike-1g    6h37m
spike-4g    6h37m
spike-gpu   6h4m
```

All four `spike-*` carry label `swift.kubeswift.io/spike=phase-0` (verified via `kubectl get -l swift.kubeswift.io/spike=phase-0 swiftguestclass`).

| Name | Verdict | Reason |
|---|---|---|
| `default` | **PRESERVE** | Used by `faas-test`, `sample`, `qemu-test`. Production. |
| `spike-1g` / `spike-4g` / `spike-16g` | **DELETE** | From `hack/snapshot-spike/01-spike-guests.yaml` — pause-window curve classes. No live SwiftGuest references them (verified: all running guests use `default`). |
| `spike-gpu` | **DELETE** | From `hack/snapshot-spike/04-gpu-spike-guest.yaml`. |

### 1.3 SwiftImages

**Command:** `kubectl get swiftimage -A`

```
NAMESPACE   NAME                AGE
default     ubuntu-noble        12d
default     ubuntu-noble-qemu   26m
```

| Name | Verdict | Reason |
|---|---|---|
| `ubuntu-noble` | **PRESERVE** | Production canonical Ubuntu image (matches `config/samples/disk-boot/swiftimage-ubuntu-noble.yaml`). |
| `ubuntu-noble-qemu` | **AMBIGUOUS — ASK** | Created 26 min ago by `make smoke-test` qemu-boot scenario. Re-importable from sample if needed. Operator decides. |

### 1.4 SwiftKernels

```
NAMESPACE   NAME           PROFILE        PHASE   AGE
default     faas-minimal   faas-minimal   Ready   12d
```

| Name | Verdict | Reason |
|---|---|---|
| `faas-minimal` | **PRESERVE** | Production kernel artifact for the kernel-boot scenario. |

### 1.5 SwiftSeedProfiles

```
NAMESPACE   NAME             AGE
default     minimal          12d
default     qemu-test-seed   26m
```

| Name | Verdict | Reason |
|---|---|---|
| `minimal` | **PRESERVE** | Production seed. |
| `qemu-test-seed` | **AMBIGUOUS — ASK** | Smoke-test fixture, recreated on each smoke-test run. Operator decides. |

### 1.6 SwiftSnapshot / SwiftRestore

**Command:** `kubectl api-resources --api-group=snapshot.kubeswift.io`

Result: empty. **CRDs are not installed on this cluster yet** — Phase 1 (PR #6) hasn't been merged + deployed. Therefore zero SwiftSnapshot or SwiftRestore artifacts exist; nothing to clean up.

This is consistent with the deployed controller image being `ghcr.io/projectbeskar/kubeswift/controller-manager:v0.2.0-rc.1` (pre-Phase-1).

### 1.7 SwiftGuestPool

**Command:** `kubectl get swiftguestpool -A` → `No resources found`.

Nothing to clean up.

### 1.8 SwiftGPUProfile

```
NAMESPACE   NAME             COUNT   MODEL   MODE       TIER
default     gtx1080-single   1               isolated   pcie
```

| Name | Verdict | Reason |
|---|---|---|
| `gtx1080-single` | **PRESERVE** | Created during Tier 1 GPU validation (5h ago, matches `config/samples/gpu-pcie/swiftgpuprofile-gtx1080.yaml`). The boba SwiftGPUNode shows the GPU as `allocated=false`. Production. |

### 1.9 SwiftGPUNode (cluster-scoped)

```
NAME            PHASE   GPUS   FREE   MODEL
boba            Ready   1      1      NVIDIA Corporation GP104 [GeForce GTX 1080]
mock-gpu-node
```

| Name | Verdict | Reason |
|---|---|---|
| `boba` | **PRESERVE** | Real GPU node, populated by the gpu-discovery DaemonSet. |
| `mock-gpu-node` | **DELETE** | Created 28 min ago by `make smoke-test` `gpu-alloc` scenario as a synthetic fixture (no real hardware). Carries label `kubeswift.io/gpu-node=true` but `Phase` is empty — the smoke-test patches a synthetic status. Smoke test did not finish cleanup because the qemu-boot scenario failed first. |

---

## 2. Standard Kubernetes resources from the spike

### 2.1 Pods

**Command:** `kubectl get pods -A -l swift.kubeswift.io/spike=phase-0`

```
NAMESPACE   NAME                 READY   STATUS      RESTARTS   AGE
default     ch-debug-boba        1/1     Running     0          6h32m
default     ch-debug-frida       1/1     Running     0          6h32m
default     spike-clone-writer   1/1     Running     0          5h
default     spike-gpu-manual     1/1     Running     0          5h49m
default     spike-restore-4g     1/1     Running     0          6h8m
default     spike-writer-2       1/1     Running     0          4h58m
default     spike-xns-verify     0/1     Completed   0          4h52m
```

| Name | Verdict | Reason |
|---|---|---|
| `ch-debug-boba`, `ch-debug-frida` | **DELETE** | From `hack/snapshot-spike/02-ch-debug-pod.yaml`. Privileged debug pods — should not be left around. |
| `spike-clone-writer`, `spike-writer-2` | **DELETE** | From `hack/snapshot-spike/11-section-6-writer-pod.yaml`. Continuous-write pods holding clones during deletion test. |
| `spike-gpu-manual` | **DELETE** | From `hack/snapshot-spike/05-gpu-manual-vm-pod.yaml`. Holds the GTX 1080 via VFIO; deletion releases the GPU. |
| `spike-restore-4g` | **DELETE** | From `hack/snapshot-spike/03-restore-pod.yaml`. Holds the still-`Terminating` PVC `swiftguest-root-spike-vm-4g` (see §3) — must delete this pod first, then the PVC clears its `pvc-protection` finalizer. |
| `spike-xns-verify` | **DELETE** | One-shot Completed pod from cross-namespace dataSource verification (§6a). |

**Plus stuck non-spike-labelled pod:**

| Name | Verdict | Reason |
|---|---|---|
| `swiftguest-rootclone-qemu-test-scrwm` | **DELETE** (after `qemu-test` SwiftGuest decision) | `ContainerCreating` for 22 min, blocked by faulted Longhorn volume `pvc-91757340…`. Will go away when `qemu-test` SwiftGuest is deleted. |

**Pods to PRESERVE** (running, not from spike):

```
default            faas-test                          (kernel-boot SwiftGuest launcher)
default            sample                             (smoke-test disk-boot launcher; AMBIGUOUS upstream)
kubeswift-system   controller-manager-…               (running v0.2.0-rc.1)
kubeswift-system   gpu-discovery-…                    (DaemonSet pod on boba)
```

### 2.2 PVCs

**Command:** `kubectl get pvc -A`

```
NAMESPACE          NAME                                  STATUS        SIZE   AGE
default            spike-clone-40g-from-snap             Bound         10Gi   5h46m
default            spike-clone-from-delete-policy        Bound         10Gi   4h58m
default            spike-clone-then-expand               Bound         40Gi   5h2m
default            spike-xns-clone                       Bound         1Gi    4h52m
default            swiftguest-root-qemu-test             Bound         40Gi   18m
default            swiftguest-root-sample                Bound         40Gi   28m
default            swiftguest-root-spike-vm-4g           Terminating   20Gi   6h37m
default            swiftimage-import-ubuntu-noble        Bound         10Gi   12d
default            swiftimage-import-ubuntu-noble-qemu   Bound         40Gi   26m
kubeswift-system   spike-xns-source                      Bound         1Gi    4h53m
```

| Name | Verdict | Reason |
|---|---|---|
| `spike-clone-40g-from-snap` | **DELETE** | From `hack/snapshot-spike/07-section-4-clone-pvc.yaml`. |
| `spike-clone-from-delete-policy` | **DELETE** | From §6b deletionPolicy=Delete test. |
| `spike-clone-then-expand` | **DELETE** | From `hack/snapshot-spike/10-section-5-expand-after.yaml`. |
| `spike-xns-clone` (default) | **DELETE** | From cross-namespace test §6a (the empty clone). |
| `spike-xns-source` (kubeswift-system) | **DELETE** | From cross-namespace test §6a — the ad-hoc source PVC. |
| `swiftguest-root-spike-vm-4g` | **DELETE — STUCK** | Already in `Terminating` for 28 min. Holding finalizer `kubernetes.io/pvc-protection`. `Used By: spike-restore-4g`. Will clear once `spike-restore-4g` pod is deleted. **Do not patch the finalizer manually until `spike-restore-4g` is gone.** |
| `swiftguest-root-qemu-test` | **DELETE** (after `qemu-test` SwiftGuest is deleted) | Per §1.1 decision. Note: the underlying Longhorn volume is `faulted` (§6) — Terminating may take longer than 2 min; document but do not force-delete (per task constraints). |
| `swiftguest-root-sample` | **DELETE** (after `sample` SwiftGuest is deleted) | Tied to AMBIGUOUS-ASK SwiftGuest. |
| `swiftimage-import-ubuntu-noble` | **PRESERVE** | Backing PVC for the `ubuntu-noble` SwiftImage. Owned by SwiftImage controller. |
| `swiftimage-import-ubuntu-noble-qemu` | **AMBIGUOUS — ASK** (tied to SwiftImage decision) | Backing PVC for `ubuntu-noble-qemu`. Will be reaped automatically when the SwiftImage is deleted. |

### 2.3 VolumeSnapshots

**Command:** `kubectl get volumesnapshot -A`

```
NAMESPACE          NAME             READYTOUSE   SOURCEPVC          RESTORESIZE   SNAPSHOTCLASS       AGE
kubeswift-system   spike-xns-snap   true         spike-xns-source   1Gi           longhorn-snapshot   4h52m
```

| Name | Verdict | Reason |
|---|---|---|
| `spike-xns-snap` | **DELETE** | Cross-namespace test artifact (§6a). |

The `spike-source-snap` referenced by VolumeSnapshotContent below is **already deleted at the VolumeSnapshot level** but its content survived (Retain policy) — see §2.4.

### 2.4 VolumeSnapshotContents (cluster-scoped)

**Command:** `kubectl get volumesnapshotcontent`

```
NAME                                               READYTOUSE   DELETIONPOLICY   VOLUMESNAPSHOT      VOLUMESNAPSHOTNAMESPACE   AGE
snapcontent-79550a66-29c8-4add-9211-85746ecef479   true         Retain           spike-source-snap   default                   5h46m
snapcontent-79b1806b-4803-4314-8247-9291103c190b   true         Retain           spike-xns-snap      kubeswift-system          4h52m
```

| Name | Verdict | Reason |
|---|---|---|
| `snapcontent-79550a66…` | **DELETE** | Orphaned — its `spike-source-snap` VolumeSnapshot is already gone. The content survived because of `deletionPolicy: Retain` (the spike chose this deliberately, see spike report §6). |
| `snapcontent-79b1806b…` | **DELETE** (after `spike-xns-snap` VolumeSnapshot is deleted) | Will become orphaned once VolumeSnapshot is removed (Retain policy). |

### 2.5 VolumeSnapshotClasses (cluster-scoped)

**Command:** `kubectl get volumesnapshotclass`

```
NAME                       DRIVER               DELETIONPOLICY   AGE
longhorn-snapshot          driver.longhorn.io   Retain           6h38m
longhorn-snapshot-delete   driver.longhorn.io   Delete           4h58m
```

Only `longhorn-snapshot` is in `hack/snapshot-spike/00-longhorn-volumesnapshotclass.yaml`. `longhorn-snapshot-delete` was created ad-hoc during the spike (§6b "deletionPolicy: Delete" test) and is referenced as `# Created via kubectl create volumesnapshotclass …` in the spike outputs.

| Name | Verdict | Reason |
|---|---|---|
| `longhorn-snapshot` | **AMBIGUOUS — ASK** | The Phase 1 e2e test scripts `test/snapshot/snapshot-test.sh` and `test/clonestrategy/clonestrategy-test.sh` auto-detect the cluster's default VolumeSnapshotClass, but if no default is set they accept `--vsclass <name>`. **Decision needed:** keep both classes for future test runs, or delete and recreate freshly when needed? Recommendation: **keep `longhorn-snapshot` (Retain), delete `longhorn-snapshot-delete`** — Retain is the safer baseline for hand-debugging; Delete-policy can be recreated trivially. Awaiting confirmation. |
| `longhorn-snapshot-delete` | **AMBIGUOUS — ASK** | Same decision as above. |

### 2.6 ConfigMaps

**Command:** `kubectl get cm -n default | grep -v kube-root-ca`

```
faas-test-runtime-intent   1   7h41m
qemu-test-runtime-intent   1   18m
qemu-test-seed             3   18m
sample-runtime-intent      1   28m
sample-seed                3   28m
```

| Name | Verdict | Reason |
|---|---|---|
| `faas-test-*` | **PRESERVE** | Owned by the production `faas-test` SwiftGuest. |
| `sample-*` | **AMBIGUOUS — ASK** (tied to `sample` SwiftGuest) | Owned by `sample`. Will be reaped automatically with the SwiftGuest. |
| `qemu-test-*` | **AMBIGUOUS — ASK** (tied to `qemu-test` SwiftGuest) | Same as above. |

No ConfigMaps in `kubeswift-system` outside of system-default. No spike-labelled ConfigMaps found.

### 2.7 ServiceAccounts / RBAC drift in `default`

**Command:** `kubectl get sa,rolebinding,role -n default | grep -E spike`

Result: empty. No spike-related RBAC drift.

---

## 3. Stuck finalizer detail

### `swiftguest-root-spike-vm-4g` PVC

**Command:** `kubectl describe pvc swiftguest-root-spike-vm-4g -n default`

```
Status:        Terminating (lasts 28m)
Finalizers:    [kubernetes.io/pvc-protection]
Used By:       spike-restore-4g
```

This is the standard kube-controller-manager PVC protection finalizer. It will clear once `spike-restore-4g` pod is deleted (§2.1). **No manual finalizer patching needed** — this is the documented happy-path Terminating sequence.

---

## 4. HostPath remnants on nodes

**Command:** `kubectl exec ch-debug-{boba,frida} -n default -- ls -la /tmp/snap/`
(the `ks-snap` volume on those pods is `hostPath: /var/lib/kubeswift/spike-snapshots/`)

### boba — `/var/lib/kubeswift/spike-snapshots/`

```
total 4892
drwxr-xr-x 4 root root    4096 Apr 25 10:35 .
drwxrwxrwt 1 root root    4096 Apr 25 10:27 ..
-rwxr-xr-x 1 root root 4989648 Apr 25 10:35 cloud-hypervisor      ← copy of CH binary
drwxr-xr-x 2 root root    4096 Apr 25 10:27 spike-vm-1g           ← snapshot dir
drwxr-xr-x 2 root root    4096 Apr 25 10:35 spike-vm-4g           ← snapshot dir
```

### frida — `/var/lib/kubeswift/spike-snapshots/`

```
total 12
drwxr-xr-x 3 root root 4096 Apr 25 10:28 .
drwxrwxrwt 1 root root 4096 Apr 25 10:29 ..
drwxr-xr-x 2 root root 4096 Apr 25 10:28 spike-vm-16g
```

### miles

Not yet probed (no ch-debug pod on miles). The spike never touched miles directly (controller-manager runs on miles but does not write to `/var/lib/kubeswift/spike-snapshots/`). Will confirm during Step 2 cleanup via a one-off privileged pod on miles.

| Path | Node | Verdict | Reason |
|---|---|---|---|
| `/var/lib/kubeswift/spike-snapshots/` (whole subtree) | boba | **DELETE** | Spike-only directory; safe to nuke as a tree. |
| `/var/lib/kubeswift/spike-snapshots/` (whole subtree) | frida | **DELETE** | Same. |
| `/tmp/snap`, `/tmp/restore`, `/tmp/<vm>.sock` | both ch-debug pods | **DELETE — automatic** | These are *inside* the ch-debug pod's own container filesystem (the symlinks resolve to `/host/kubelet-pods/...`), not on the host. Pod deletion clears them. |

The per-guest **runtime directories** the controller normally creates under `/var/lib/kubeswift/run/<ns>-<guest>/` for the spike SwiftGuests are kubelet-pod-scoped emptyDirs (verified — the launcher pod template uses `emptyDir: {}` for the `run` volume, not hostPath). Kubelet GCs these when the pod is deleted; **no manual cleanup needed.**

---

## 5. Cluster-environment diagnostics (DO NOT FIX — for environmental report)

### 5.1 Longhorn unhealthy volumes

`kubectl get volumes.longhorn.io -A`:

| PVC name | State | Robustness | Used by |
|---|---|---|---|
| `pvc-043d81a2…` | attached | **degraded** | `spike-clone-writer` |
| `pvc-1665bc7b…` | attached | **degraded** | `sample` (production-ish) |
| `pvc-17f7a332…` | detached | unknown | (nothing live; `spike-xns-source` PVC) |
| `pvc-24e6cf27…` | attached | **degraded** | `spike-writer-2` |
| `pvc-2ece6b88…` | detached | unknown | (`spike-xns-clone` PVC) |
| `pvc-470f9654…` | attached | **degraded** | `spike-restore-4g` |
| `pvc-5870de39…` | detached | unknown | (`spike-clone-then-expand`) |
| `pvc-6bf9f5db…` | attached | **degraded** | `swiftimage-import-ubuntu-noble-qemu` Job |
| `pvc-91757340…` | detached | **faulted** | `swiftguest-root-qemu-test` (engine has 0 replicas in spec) |
| `pvc-cb90f2e9…` | detached | unknown | (`swiftimage-import-ubuntu-noble`) |

**Root cause** (`kubectl -n longhorn-system get nodes.longhorn.io boba -o json | jq .status.diskStatus`):

```json
"default-disk-3626c798d41cd46e": {
  "conditions": [
    {"type": "Ready",       "status": "False",
     "reason": "DiskFilesystemChanged",
     "message": "Disk default-disk-3626c798d41cd46e(/var/lib/longhorn/) on node boba is not ready: record diskUUID doesn't match the one on the disk "},
    {"type": "Schedulable", "status": "False",
     "reason": "DiskNotReady",
     "message": "Disk default-disk-3626c798d41cd46e (/var/lib/longhorn/) on the node boba is not ready"}
  ],
  "storageAvailable": 0, "storageMaximum": 0
}
```

**Boba's Longhorn disk is unschedulable** because the diskUUID Longhorn recorded does not match the diskUUID currently present on the filesystem at `/var/lib/longhorn/`. Almost certainly: the disk under `/var/lib/longhorn/` was wiped/reformatted/replaced after Longhorn first registered the node (boba was added to the cluster 6h50m ago — same timeframe as the spike).

Result: Longhorn's `default-replica-count=3` cannot be honored — boba can't take replicas, so every new volume gets `degraded` (2 of 3 replicas), and any volume whose first replica attempt happened to land *only* on boba ends up `faulted` (0 replicas — see `pvc-91757340-…` engine: `replicas: null`).

This is **not** a KubeSwift bug. It will be covered in `cleanup-environmental-issues-2026-04-25.md` with the Longhorn-recommended remediation (delete and re-add the disk on boba via a Longhorn Node CR patch). **Step 2 deletion order avoids touching this** — just delete the spike PVCs, and let the orphaned `faulted` qemu-test PVC clean itself up after the SwiftGuest deletion patches its finalizer.

### 5.2 boba GPU passthrough state — ALREADY CLEAN

`kubectl exec gpu-discovery-sk4m9 -n kubeswift-system -- lspci -nnk -s 0000:01:`:

```
01:00.0 VGA compatible controller [0300]: NVIDIA Corporation GP104 [GeForce GTX 1080] [10de:1b80] (rev a1)
	Subsystem: NVIDIA Corporation Device [10de:119e]
	Kernel driver in use: vfio-pci

01:00.1 Audio device [0403]: NVIDIA Corporation GP104 High Definition Audio Controller [10de:10f0] (rev a1)
	Subsystem: NVIDIA Corporation Device [10de:119e]
	Kernel driver in use: vfio-pci
```

Both GPU and audio peer are bound to `vfio-pci`. SwiftGPUNode `boba.status.gpus[].allocated=false`. **No driver-override mismatch, no rebind required.** The state is exactly what a clean VFIO setup looks like — likely because the `spike-gpu-manual` pod (still running) has been holding `0000:01:00.0` via VFIO continuously since the spike, and the controller-side allocation is intentionally still `false` (the manual pod is a hand-rolled CH+VFIO pod, not a SwiftGuest, so it bypasses the SwiftGPU allocator).

When `spike-gpu-manual` is deleted, both devices stay bound to vfio-pci (that's the whole point of `driver_override` once set). **No cleanup needed.**

The environmental report will still document the current state for the record but with the recommendation **"do nothing — already clean."**

---

## 6. Anything I might have missed

The user's task description mentioned naming patterns `e2e-snapshot-*`, `e2e-clonestrategy-*`, `pool-*` for Phase 1 e2e test runs. **None of those exist** because:

1. SwiftSnapshot/SwiftRestore CRDs aren't installed (PR #6 hasn't merged).
2. The deployed controller image is `v0.2.0-rc.1` — no Phase 1 logic running.
3. The new Phase 1 e2e scripts (`test/snapshot/`, `test/clonestrategy/`) explicitly require a snapshot-capable + healthy storage layer, which this cluster currently does not have (boba's Longhorn disk is unschedulable; see §5.1).

So Phase 1 e2e cleanup is **not needed** in this pass. After the cluster is repaired and Phase 1 ships to ghcr, those tests can be run cleanly.

---

## Recommendation: Step 2 deletion order

Once authorized, suggested order (matches the user's task description):

1. **Pods (default ns)** with label `swift.kubeswift.io/spike=phase-0` (7 pods). Plus `swiftguest-rootclone-qemu-test-scrwm` (depends on §1.1 decision).
2. **mock-gpu-node** SwiftGPUNode (cluster-scoped).
3. (If §1.1 approved) `qemu-test` and `sample` SwiftGuests (default ns) + their owned ConfigMaps + their per-guest root-disk PVCs.
4. **Spike-labelled SwiftGuestClasses** (`spike-1g`/`-4g`/`-16g`/`-gpu`).
5. **Spike PVCs** (4 in default + 1 in kubeswift-system, by label `swift.kubeswift.io/spike=phase-0`).
6. **`swiftguest-root-spike-vm-4g`** Terminating PVC will clear automatically after `spike-restore-4g` pod is gone.
7. **VolumeSnapshot** `spike-xns-snap` (kubeswift-system).
8. **VolumeSnapshotContents** orphaned: `snapcontent-79550a66-…` and `snapcontent-79b1806b-…`.
9. (If approved) **VolumeSnapshotClasses** `longhorn-snapshot-delete` (drop the Delete-policy one, keep `longhorn-snapshot` for future test runs).
10. (If §1.4 approved) `ubuntu-noble-qemu` SwiftImage and `qemu-test-seed` SwiftSeedProfile.
11. **HostPath cleanup** via one-off privileged debug DaemonSet:
    - boba: `rm -rf /var/lib/kubeswift/spike-snapshots`
    - frida: `rm -rf /var/lib/kubeswift/spike-snapshots`
    - miles: confirm absent; if present, `rm -rf /var/lib/kubeswift/spike-snapshots`
12. **Verification**: `make smoke-test` to confirm baseline cluster works.

Total resources tagged for deletion: **~25 cluster-scoped + namespaced objects**, plus 2 hostpath subtrees on boba/frida.
Total resources tagged AMBIGUOUS — ASK: **5** (sample SwiftGuest, qemu-test SwiftGuest, ubuntu-noble-qemu SwiftImage, qemu-test-seed SwiftSeedProfile, both VolumeSnapshotClasses).

Stopping here for human review. Step 2 will not start until the AMBIGUOUS items are resolved and the inventory is approved.
