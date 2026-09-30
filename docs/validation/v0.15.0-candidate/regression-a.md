# Regression run, Phase A: the v0.15.0 upgrade's result

Run 2026-09-30, 09:28–09:35 UTC, read-only. Node names are generalised
(`cp-1`, `worker-1`, `worker-2`; ntx `<n1>`–`<n3>`).

## Summary

| Check | dev | ntx | sov |
|---|---|---|---|
| Helm | **PASS**: rev 51 `deployed` `kubeswift-0.15.0` (09:11:25); nothing between 50 and 51 | **PASS**: rev 33 (09:13:17); nothing between 32 and 33 | **PASS**: rev 39 (09:12:51); nothing between 38 and 39 |
| Values kept (user values) | **PASS**: only the nine `image.tag` | **PASS**: only the nine `image.tag` | **PASS**: only the nine `image.tag` |
| **Values kept (computed)** | **PASS** | **FINDING A1**: `metrics.secure` `false` → `true` | **FINDING A1**: the same |
| CRDs | **PASS**: no diff | **PASS** | **PASS** |
| Images | **PASS**: all `v0.15.0` (UI `v0.12.4`), 0 restarts | **PASS**: controller `v0.15.0`, 0 restarts (+32 dead pods from 2026-09-29, A3) | **PASS** |
| Controller | **PASS**: 13 workers, 0 errors, `--metrics-secure=true`, can list events | **PASS**: 13 workers, 10 routine conflict errors, can list events; `--metrics-secure=true` (A1) | **PASS**: 13, 0 errors, can list events; `--metrics-secure=true` (A1) |
| VAPs and RBAC | **PASS**: 4 VAPs; console grant; vm-reader without `pods/exec` | **PASS**: 3 VAPs (no gateway) | **PASS**: 4 VAPs; console grant; vm-reader without `pods/exec` |
| SwiftKernels | **PASS**: 4 `Ready` | **PASS**: `ft-faas` `Ready` | none |
| Existing guests | **PASS** | **FINDING A2**: both CAPI guests `Failed` since 2026-09-29 12:57, evicted by the kubelet under disk pressure **before** the upgrade | none |
| Left over | none | `val-n2` (the round-1 leftover guest's namespace), expected | none |

**Every controller is healthy and every kubeswift image is `v0.15.0`**, so
Phases B and C go ahead on all three clusters. A1 and A2 are findings.
**A2 means the ntx CAPI workload cluster's VMs have been down for about 20
hours.** Per the plan, `capi-udn` is left untouched.

## Findings

**A1: ntx and sov switched to secure controller metrics.**
- **What changed.** Their full computed values differ from the round-7
  revision in two keys besides the nine image tags:

```text
ntx (rev 32 -> 33):  controllerManager.metrics.secure: False -> True ;  controllerManager.metrics.readers: (absent) -> []
sov (rev 38 -> 39):  controllerManager.metrics.secure: False -> True ;  controllerManager.metrics.readers: (absent) -> []
dev (rev 50 -> 51):  nothing but the nine image tags
rendered args: ntx and sov  rev 32/38 --metrics-secure=false  ->  rev 33/39 --metrics-secure=true
```

- **Why the plan's check missed it.** Neither cluster sets
  `controllerManager.metrics.*` in its user values, and the plan's
  `helm get values` diff shows user values only.
- **The chart default** is `metrics.secure: true` in both
  `0.0.0-dev.8afbad3` and `0.15.0`. Until round 7 the clusters rendered
  `false`, because `helm upgrade --reuse-values` carries forward the *old*
  release's coalesced chart defaults, from an install where the default was
  `false`.
- **So this upgrade did not reuse values that way.** It behaved like
  `--reset-then-reuse-values`, `--reset-values`, or `-f <saved user values>`.
  The superseded upgrade section warned that those "would apply 0.15.0's
  chart defaults … for example, secure metrics on ntx and sov".
- **Effect.** The controller metrics endpoint on ntx and sov now needs an
  authorised token, and no readers are bound (`readers: []`). Anything that
  scraped it over plain HTTP no longer can. Switching them was to be
  William's call.

**A2: ntx's CAPI guests are down, evicted on 2026-09-29, before the upgrade.**

```text
capi-udn/ks-udn-cp-54klw         Failed  GuestRunning=False/LauncherExited @2026-09-29T12:57:54Z  PodScheduled=False "Evicted"
  launcher pod uid a5420720-… (the same as round 7) on <n1>: phase Failed, restartPolicy Never,
  launcher lastState terminated ContainerStatusUnknown exit 137; restarts 1
capi-udn/ks-udn-md0-sz2ql-bz9zs  Failed  GuestRunning=False/LauncherExited @2026-09-29T12:57:55Z  PodScheduled=False "Evicted"
  launcher pod uid f598c58f-… on <n3>: the same shape
```

- **When.** The failure is about 20 h before the 09:13 upgrade. It matches
  the ntx disk-pressure event in A3.
- **The new controller** only wrote `EgressReady=False/LauncherExited` at
  09:31, which is why the resourceVersions moved.
- **They were not relaunched.** Their `runPolicy` is unset, and nothing has
  recreated a launcher since the eviction.
- **Left as they are:** `capi-udn` is hands-off.

**A3: ntx had a disk-pressure event on 2026-09-29, and it is still tight.**

```text
controller-manager (old ReplicaSet, sha-8afbad3) pods left behind: 32
  30 × Failed Evicted "Pod was rejected: The node had condition: [DiskPressure]"   <n1>, 12:47:56–12:48:01 (a rejection storm)
   1 × Failed Evicted "The node was low on resource: ephemeral-storage …"            <n3>, 11:20
   2 × Succeeded (the pods replaced by later rollouts)
nodes now: <n1> DiskPressure=False since 2026-09-30T00:05:33Z; <n2> since 2026-09-29T13:50:05Z; <n3> since 2026-09-29T13:31:43Z
root fs now: <n1> 7.3 GiB free of 23 (31%), <n2> 9.7 (41%), <n3> 8.2 (34%)
Longhorn disks: <n1> Schedulable=False, <n2> Schedulable=False, <n3> True
```

- **Not a v0.15.0 problem.** The dead pods from the event have not been
  garbage-collected.
- **Phase C watches the disk.** It checks the headroom before each ntx
  scenario. An in-place restore writes the memory snapshot to the node's
  root filesystem.

## Details

**Helm history**, the last revisions:

```text
dev  50 superseded kubeswift-0.0.0-dev.8afbad3 (2026-09-28 08:31)   51 deployed kubeswift-0.15.0 (2026-09-30 09:11:25)
ntx  32 superseded kubeswift-0.0.0-dev.8afbad3 (2026-09-28 08:32)   33 deployed kubeswift-0.15.0 (2026-09-30 09:13:17)
sov  38 superseded kubeswift-0.0.0-dev.8afbad3 (2026-09-28 08:32)   39 deployed kubeswift-0.15.0 (2026-09-30 09:12:51)
```

**User-values diff**
(`diff <(helm get values --revision <round-7 rev> -o yaml) <(helm get values -o yaml)`).
It is the same on all three clusters: 18 changed lines, 9 keys.

```text
controllerManager.image.tag, swiftletd.image.tag, sandboxMaterialize.image.tag, snapshotORAS.image.tag,
snapshotS3.image.tag, migrationStunnel.image.tag, gpuDiscovery.image.tag, dra.image.tag, gateway.image.tag:
    sha-8afbad3 -> v0.15.0
dev: ui.image.tag v0.12.4 (unchanged); controllerManager.metrics {secure: true, readers: [monitoring/…prometheus]} (unchanged)
ntx, sov: ui.image.tag and controllerManager.metrics not set (see A1)
```

**CRDs.** `kubectl diff --server-side -f charts/kubeswift/crds/` from the
`v0.15.0` worktree shows no difference on any cluster.

**kubeswift pods:**

```text
dev: controller-manager started 09:11:31, gateway 09:11:31, gpu-discovery 09:11:33, kubeswift-dra-driver 09:11:33: all :v0.15.0, 0 restarts;
     kubeswift-ui :v0.12.4 (started 2026-09-11), 0 restarts
ntx: controller-manager started 09:13:19, :v0.15.0, 0 restarts   (+ the 32 dead pods of A3)
sov: controller-manager started 09:12:56, :v0.15.0, 0 restarts
```

**Controller errors since start:**
- dev: 0.
- sov: 0.
- ntx: 10, all of one kind: `Reconciler error … Operation cannot be fulfilled
  on swiftkernels.kernel.kubeswift.io "ft-faas": the object has been
  modified`. This is the routine optimistic-lock conflict seen in earlier
  rounds.

**Existing guests and their launcher images:**

| Guest | Phase | Launcher | Image |
|---|---|---|---|
| dev `gpu-cells/innercp` | Running | uid `10bd28b8-…`, 0 restarts | `swiftletd:v0.13.15` |
| dev `field-testing/ft-gpu-pool-slot-j74r2` (warm slot) | Running | uid `6aeee96c-…`, 0 restarts; GPU `0000:01:00.0` allocated to it | `swiftletd:sha-08d0165` |
| ntx `capi-udn/ks-udn-cp-54klw` | **Failed** (A2) | uid `a5420720-…`, restarts 1, pod Failed | `swiftletd:v0.13.14` |
| ntx `capi-udn/ks-udn-md0-sz2ql-bz9zs` | **Failed** (A2) | uid `f598c58f-…`, restarts 1, pod Failed | `swiftletd:sha-d232581` |
| ntx `default/sample`, `val-n2/snapshot-local-source` | Failed (leftovers, "SwiftImage failed: import job failed") | none | none |

**Controller log capture.** `kubectl logs -f` of each controller, reattaching
on a pod change, has run since 09:28:28 on all three clusters. It is
summarised at the end of the run.
