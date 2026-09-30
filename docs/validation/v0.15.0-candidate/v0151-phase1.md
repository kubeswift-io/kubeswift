# v0.15.1 round, Phase 1: upgrade to the candidate `5968241`

Run 2026-09-30, 15:55–16:07 UTC. Node names are generalised as before.

| Cluster | Verdict |
|---|---|
| dev | **PASS**: Helm 51 → 52 (`0.0.0-dev.5968241`) |
| ntx | **PASS**: Helm 33 → 34, and **V1 PASS**: the CAPI guests stop churning |
| sov | **PASS**: Helm 39 → 40 |

## Candidate and build check

- **The candidate.** `5968241` is on main.
- **Since `v0.15.0` it carries:**
  - #696, #697, #698, #699 and #700, as the plan lists;
  - **two dependency bumps the plan's table does not mention:** #688, the
    kubernetes group, with `k8s.io/*` now at `v0.37.1`, and #689,
    go-minor-patch.
- **Scope.** `git diff v0.15.0 5968241` touches the CRDs `swiftguests`,
  `swiftguestclasses` and `swiftguestpools` (the storage rule, 2 lines each),
  and no RBAC, chart template or Rust.
- **CI.** `CI`, `Release Dev`, `SAST` and `Security` for `5968241` all
  succeeded. At 15:55:48 the chart `0.0.0-dev.5968241` and all nine
  `sha-5968241` images were present.

## Pre-checks and baselines (15:56)

| | dev | ntx | sov |
|---|---|---|---|
| Stopped-but-Running | none (also right before the upgrade) | none | none |
| `Failed` guests | none | `capi-udn/ks-udn-cp-54klw`, `capi-udn/ks-udn-md0-sz2ql-bz9zs` (both evicted on 2026-09-29, launcher pods `Failed`); `default/sample`, `val-n2/snapshot-local-source` ("SwiftImage failed", no pod) | none |
| In progress | nothing | nothing (`val-n2` is the round-1 leftover; `val-reg-smoke` is the regression run's N1, kept as BLOCKED) | nothing |
| Baselines | `innercp` launcher `10bd28b8-…`, 0 restarts, `swiftletd:v0.13.15`; `ft-gpu-pool-slot-j74r2` `6aeee96c-…`, 0 restarts, holding the GPU | CAPI launchers `a5420720-…` and `f598c58f-…`, `Failed`, restarts 1 | none |
| Controller | `v0.15.0`, `--metrics-secure=true` | `v0.15.0`, `--metrics-secure=true` | `v0.15.0`, `--metrics-secure=true` |

**ntx's Longhorn is still down,** as at the end of the regression run:
- one `longhorn-manager` in CrashLoopBackOff with 216 restarts, another with
  217;
- Longhorn nodes `<n1>` and `<n2>` read `Ready=False/ManagerPodDown`;
- the regression run's N1 PVC is still `Pending`.

## Upgrade (dev 15:59, ntx 15:59, sov 16:05)

- **CRDs.** Applied from the `5968241` worktree: 15 `serverside-applied` on
  each cluster.
- **Helm.** `helm upgrade kubeswift oci://ghcr.io/kubeswift-io/charts/kubeswift
  --version 0.0.0-dev.5968241 --reuse-values` with the nine
  `--set <c>.image.tag=sha-5968241`.
- **The dry run passed on each cluster:**
  - exit 0;
  - every kubeswift image `sha-5968241`;
  - no `v0.15.0` image left, and no `:latest`;
  - `--metrics-secure=true` in the render.
- **Rollout.** `rollout status` for every Deployment and DaemonSet:
  - dev: all five by 15:59:39;
  - ntx: `controller-manager` by 16:00:26;
  - sov: by 16:06:12.

## After the upgrade

| | dev | ntx | sov |
|---|---|---|---|
| Helm | rev 52, `kubeswift-0.0.0-dev.5968241`, deployed | rev 34 | rev 40 |
| User values | 18 lines: the nine `image.tag` `v0.15.0` → `sha-5968241`, nothing else | the same | the same |
| Computed values (`--all`) | 9 changed, all `image.tag` | the same | the same |
| Images | `controller-manager`, `gateway`, `gpu-discovery`, `kubeswift-dra-driver` on `sha-5968241`, 0 restarts; UI `v0.12.4` | `controller-manager` `sha-5968241`, 0 restarts | the same |
| Controller | 13 "Starting workers", 0 errors, `--metrics-secure=true` | 13, `--metrics-secure=true`; 2 errors, both the routine `swiftkernels "ft-faas": the object has been modified` conflict | 13, 0 errors, `--metrics-secure=true` |
| VAPs | the same 4 | the same 3 | the same 4 |
| CRD rule on the cluster | `!(has(self.accessMode) && self.accessMode == 'ReadWriteMany' && (!has(self.volumeMode) \|\| self.volumeMode == 'Filesystem'))` | the same | the same |
| SwiftKernels | 4 × `Ready` | `ft-faas` `Ready` | none |
| Baselines | `innercp` `10bd28b8-…` 0 restarts; slot `6aeee96c-…` 0 restarts, GPU still allocated to it | the CAPI launchers unchanged (same uids, `Failed`, restarts 1); the leftovers unchanged | none |

## V1: churn stops (#696), ntx. PASS

The two CAPI guests are still `Failed`, with their evicted launcher pods, so
V1 runs.

**Before, on v0.15.0**, sampled every 35 s:

```text
15:57:04 ks-udn-cp-54klw rv=33993287 EgressReady=False/LauncherExited@15:57:02 | ks-udn-md0 rv=33993285 @15:57:02
15:57:39 ks-udn-cp-54klw rv=33993458 EgressReady=False/LauncherExited@15:57:38 | ks-udn-md0 rv=33993460 @15:57:38
15:58:14 ks-udn-cp-54klw rv=33993625 EgressReady=False/LauncherExited@15:58:11 | ks-udn-md0 rv=33993627 @15:58:11
```

**After**, sampled every 30 s for 5 minutes. The new controller
(`sha-5968241`) started at 16:00:02, with 13 workers by 16:00:26.

```text
16:00:26 ks-udn-cp-54klw rv=33994334 EgressReady=False/LauncherExited@2026-09-30T16:00:21Z PodScheduled=False/PodNotScheduled | ks-udn-md0 rv=33994335 @16:00:21
16:00:57 … 16:05:27 (11 samples): the same rv=33994334 / 33994335, EgressReady lastTransitionTime 16:00:21, every time
```

- **The churn has stopped.** The resourceVersion does not move, and
  `EgressReady` reads `False/LauncherExited` with its `lastTransitionTime`
  fixed.
- **`PodScheduled`** stays `False/PodNotScheduled` ("Evicted"). This launcher
  was evicted (`Failed`), not `Succeeded`, so the `LauncherExited` reason for
  `PodScheduled` does not apply here. F1(a) checks it on a `Succeeded`
  launcher.
