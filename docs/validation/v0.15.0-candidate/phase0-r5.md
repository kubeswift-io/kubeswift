# Phase 0 r5: free Longhorn space on dev's cp-1

Run 2026-09-25, 22:45–22:55 UTC, then step 4 at 23:40–23:44 UTC after William
approved it. Node names are generalised: `cp-1`, `worker-1`, `worker-2`.

**Verdict: DONE. cp-1 is at 60.9% free (target 35%), and a fresh
`longhorn-migratable` volume comes up `healthy` with 3 replicas.**
- **Steps 1–3 got only to 30.9%.** I stopped there and reported, as the plan
  says.
- **The bulk was orphans.** 58.7 GiB of cp-1's disk was Longhorn **orphaned
  replica data** from 2026-08-10.
- **Step 4 needed a second OK.** My session's permission check refused the
  orphan deletion at first. William then approved it explicitly (23:40), and
  deleting the 7 cp-1 orphans freed the 58.7 GiB.

## cp-1 free space (Longhorn's view of `/var/lib/longhorn/`)

| When (UTC) | Free | Schedulable |
|---|---|---|
| 22:45:47, before | 46.5 GiB of 195 GiB, **23.8%** | `False/DiskPressure` |
| 22:47:26, after step 1 | 57.8 GiB, **29.5%** | `True` |
| 22:50:59, after steps 2–3 | 60.5 GiB, **30.9%** | `True` |
| 23:41:00, after step 4 | 119.1 GiB, **60.9%** | `True` |
| Target | ≥ 68.3 GiB, **35%** | |

At 23:44 the other two nodes read: worker-1 58.4% free, worker-2 65.7% free.

Earlier, at 22:25, William authorised deleting `val-r4-mig/v3a`, `v3b` and
`v4p`. That is recorded in `phase2-r4.md`, and those three are not repeated here.

## Deleted

**Step 1: round 4 leftovers.**
- **Saved first:** the objects and events of every namespace.
- **22:46:10:** `kubectl delete ns val-r4-mig val-r4-d7b2 val-r4-rt val-r4-nok val-r4-smoke`.
- **They held:**
  - guests `t3g`, `e2e-guest` and `snapshot-local-source`;
  - 9 SwiftMigrations;
  - a local SwiftSnapshot and a SwiftRestore;
  - 4 SwiftImages and the SwiftKernel `sandbox`;
  - their PVCs.
- **#675 re-check: every namespace finished deleting.**

```text
22:46:11 all five Terminating
22:46:19 val-r4-nok, val-r4-smoke gone
22:46:26 val-r4-d7b2 gone; cleanup pod kubeswift-system/swift-snap-cleanup-snapshot-local-mem-<hash> Running (val-r4-rt's local snapshot)
22:46:30 val-r4-rt gone; the cleanup pod gone
22:46:46 val-r4-mig gone   (36 s for all five)
```

- **Freed on cp-1:** about 10.7 GiB of replica data (the `t3g` root, the R2
  root, and two image-import disks).

**Step 2: other validation leftovers.**
- There are no other `val-*` namespaces.
- `SwiftGuestClass val-migratable-16g` was deleted. Nothing used it, and its
  YAML was saved.

**Step 3: unused images.** Before deciding, I checked references across
`swiftguests,swiftguestpools,swiftsandboxes,swiftsandboxpools -A`. The only
references are `gpu-cells/innercp` → `ubuntu-noble` (in `gpu-cells`) and the
field-testing pool.

| Image | Storage | On cp-1 | Action |
|---|---|---|---|
| `default/ubuntu-noble` (created 2026-08-10) | PVC 10Gi `longhorn` | 2.5 GiB | **Deleted** 22:49:35; its PVC and Longhorn volume are gone |
| `default/ubuntu-noble-ceph` (created 2026-08-16) | PVC 6Gi `ceph-block` + a clone-seed VolumeSnapshot | none | **Kept.** Ceph's OSD on cp-1 is a fixed-size 25 GiB loop file (`/var/lib/rook-osd/osd0.img`), so deleting RBD data returns nothing to cp-1's filesystem |
| `gpu-cells/gpu-worker-noble` | PVC `longhorn` | 6.3 GiB | **Kept.** It is unreferenced, but it is in `innercp`'s namespace, which the plan protects. William's call |

## Step 4: orphaned Longhorn replicas. Done after William's approval

Longhorn has 7 `orphans.longhorn.io` objects on cp-1 (type `replica`).
- **Their volumes are gone.** For each one there is no Longhorn volume, no PV
  and no Replica object naming its data directory.
- **They are old.** Longhorn detected all 7 on **2026-08-10 09:08**, 46 days
  ago, well before this validation. `orphan-resource-auto-deletion` is unset.
- **Sizes** were measured with `du` on the node:

| Orphaned data directory (`/var/lib/longhorn/replicas/…`) | Size |
|---|---|
| `pvc-fbad34b3-bd72-4501-ba53-1c592e37d9ba-4509d5d0` | 32.1 GiB |
| `pvc-04328bfd-0ae4-4404-bbbf-139c99f297f8-57497763` | 11.6 GiB |
| `pvc-684db160-c0f7-4031-b28d-3d9ef54e8780-5ee93351` | 6.3 GiB |
| `pvc-54189ede-1c0d-43ec-9d19-84f1d97ac4ce-682419aa` | 2.7 GiB |
| `pvc-282a3785-789c-4dcf-abcb-ea5e69fb124a-ce544e51` | 2.5 GiB |
| `pvc-091f3e29-d6f8-4ddc-a0e0-65055f2c7e95-ebfe456c` | 2.5 GiB |
| `pvc-2323975d-3bd8-4e00-9862-d3c55b5dddb8-8ab0239c` | 0.8 GiB |
| **Total** | **58.7 GiB** |

- **23:40:29:** William approved, and I deleted the 7 Orphan objects:
  `kubectl -n longhorn-system delete orphans.longhorn.io <the 7 names>`. That
  is Longhorn's own way of removing orphaned data. Their definitions were
  saved first.
  - 23:40:30: 0 orphans left on cp-1.
  - 23:41:00: cp-1 at 119.1 GiB free (60.9%), exactly the measured 58.7 GiB
    more.
- **Worker-2 still has 7 orphans** from the same 7 volumes. They do not
  affect cp-1, and they were left as they are.

## What is left on cp-1's disk

Measured before step 4. `df`: 135.0 GiB used of 195.6 GiB, measured with a read-only
`kubectl debug node/<cp-1>` pod (busybox `du`/`df`), deleted afterwards.

| Path | Size | What |
|---|---|---|
| `/var/lib/longhorn` | 78.8 GiB | 58.7 GiB orphans (deleted in step 4) + ~20 GiB live replicas (below) |
| `/var/lib/rook-osd` | 25.0 GiB | the Ceph OSD loop file (fixed size) |
| `/var/lib/k0s` | 23.7 GiB | k0s: containerd images and state |
| `/var/log` | 3.6 GiB | |
| `/usr` | 2.9 GiB | |
| `/var/lib/kubeswift` | 63 MiB | `snapshots/`, not inspected |

Live Longhorn replicas on cp-1:

| Owner PVC | Actual size |
|---|---|
| `gpu-cells/swiftguest-root-innercp` | 8.2 GiB |
| `gpu-cells/swiftimage-import-gpu-worker-noble` | 6.3 GiB |
| `gpu-cells/swiftimage-import-ubuntu-noble` | 2.8 GiB |
| `field-testing/swiftimage-import-ubuntu-noble` | 2.6 GiB |
| `keycloak/keycloak-data` | 0.2 GiB |

## Fresh-volume check. PASS

A 1 GiB `longhorn-migratable` PVC (RWX, Block, 3 replicas) in a scratch
namespace `val-r5-prep`. A detached volume reports robustness `unknown`, so a
busybox pod attached it.

```text
23:42:50 PVC + pod created
23:42:53 volume detached/unknown   replicas: 3 × stopped
23:42:57 volume attaching          replicas: <worker-2> running, <cp-1> running, <worker-1> running
23:43:04 volume attached/healthy   (14 s)
23:43:15 namespace val-r5-prep deleted → gone 23:44:03; its Longhorn volume gone 23:44:04
```

## State left on dev

- **No `val-*` namespaces.** The only SwiftGuest is `gpu-cells/innercp`,
  `Running`.
- **Kept:** `default/ubuntu-noble-ceph`, `gpu-cells/gpu-worker-noble`, and
  worker-2's 7 orphans.
- **Test scripts:** `default/ubuntu-noble` is gone. They re-import it as
  needed.
- **Unchanged:**
  - every Longhorn setting;
  - `field-testing`, `capi-udn`, `kube-system` and `kubeswift-system`;
  - `innercp` and its namespace.

Ready for the round 5 candidate.
