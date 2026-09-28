# Phase 1 r6: upgrade to the candidate `4f08e87`

Run 2026-09-28, 06:27–06:32 UTC. Node names are generalised as in `phase0.md`.

| Cluster | Verdict |
|---|---|
| dev | **PASS**: Helm 48 → 49, every Deployment and DaemonSet rolled out; `ft-gpu-pool`'s running slot undisturbed |
| ntx | **PASS**: Helm 30 → 31; the two `Failed` leftovers stay `Failed` with the new "SwiftImage failed: …" message and get no launcher pod |
| sov | **PASS**: Helm 36 → 37 |

## Build check and space

- **Build check.** At 06:27:46 the chart `0.0.0-dev.4f08e87` and all nine
  `sha-4f08e87` images were present. `Release Dev`, `CI`, `E2E (cluster)`,
  `Security` and `SAST` for `4f08e87` all finished `success`.
- **What changed.** `git diff 08d0165 4f08e87` touches no CRD, RBAC, Rust or
  chart template.
- **Space.** cp-1 at 06:28:03: 124.7 GiB free (**63.7%**), `Schedulable=True`.
  No preparation needed.

## Pre-checks, on every cluster

**Pre-check 1 (runPolicy): no Stopped-but-Running guest anywhere.** Checked at
06:28:05 and again right before each upgrade (dev 06:28:46, ntx 06:29:27, sov
06:29:49).

**Pre-check 2 (`Failed` guests, #685):**

| Cluster | Guest | `Resolved` before | Launcher pods before |
|---|---|---|---|
| dev | none | | |
| ntx | `default/sample` | `False` "SwiftImage not Ready" | 0 |
| ntx | `val-n2/snapshot-local-source` | `False` "SwiftImage not Ready" | 0 |
| sov | none | | |

Both ntx guests are the expected round-1 leftovers. Their images,
`default/ubuntu-noble` and `val-n2/ubuntu-noble`, are **`Failed`**
(`ImportFailed: import job failed`, from the pre-existing ntx Longhorn attach
problem). They are not still importing.

## Commands (dev, then ntx, then sov)

- **CRDs.** Applied from a worktree at `4f08e87`: 15 `serverside-applied`, 0
  errors. They are identical to `08d0165`'s.
- **Helm.** `helm upgrade kubeswift oci://ghcr.io/kubeswift-io/charts/kubeswift
  --version 0.0.0-dev.4f08e87 --reuse-values`, with `--set <c>.image.tag=sha-4f08e87`
  for all nine components.
  - A `--dry-run` first exited 0 everywhere, with no `sha-08d0165` left.
- **Rollout.** `kubectl rollout status` for every kubeswift Deployment and
  DaemonSet.

## dev: PASS

- **Upgrade:** Helm **48 → 49** at 06:28:52. `controller-manager`,
  `kubeswift-gateway`, `kubeswift-ui`, `gpu-discovery` and
  `kubeswift-dra-driver` had rolled out by 06:29:17.
- **Images:** all nine on `sha-4f08e87`, including the controller's launcher
  image env var. The UI stays on `v0.12.4`.
- **Controller:**
  - 13 "Starting workers", 0 error lines;
  - **`--metrics-secure=true` survived** (`"secure"=true`);
  - the same 4 VAPs, all `[Deny]`.
- **Events RBAC:** a SubjectAccessReview for the controller SA, `list events`
  → `allowed`.
- **SwiftKernels:** all 4 stayed `Ready`.
- **Baseline:** `innercp`'s launcher has the same uid `10bd28b8-…`, 0 restarts,
  `Running`.

### `field-testing/ft-gpu-pool`, observed only

A 3 s watcher ran from 06:28:43 through the upgrade. It saw no change in the
pool, its slots or the GPU:

```text
before (06:28:05) slot ft-gpu-pool-slot-j74r2 uid=6aeee96c-37f2-45d8-b999-b7e42bc093ad Running restarts=0 start=2026-09-26T00:00:02Z
                  GPU 0000:01:00.0 allocated=true to=sandbox:field-testing/ft-gpu-pool-slot-j74r2
after  (06:30:59) the same uid, Running, restarts=0, the same start; GPU still allocated to it; pool Ready
```

**A controller upgrade did not disturb the running warm slot.**

## ntx: PASS

- **Upgrade:** Helm **30 → 31** at 06:29:30, rolled out by 06:29:49.
- **Images:** all `sha-4f08e87`, `--metrics-secure=false`, VAPs as before (3).
- **Controller:** 13 "Starting workers". The 2 error lines are routine
  optimistic-lock conflicts on the SwiftKernel `field-testing/ft-faas`
  ("the object has been modified"). That kernel stayed `Ready`.
- **Events RBAC (SAR):** `allowed`.

### The pre-check 2 guests after the upgrade

At 06:30:59:

```text
default/sample                phase=Failed  Resolved=False/ResolutionFailed "SwiftImage failed: import job failed"   launcher pods: 0
val-n2/snapshot-local-source  phase=Failed  Resolved=False/ResolutionFailed "SwiftImage failed: import job failed"   launcher pods: 0
```

- **They stay `Failed`, correctly.** The plan expected a guest reading "…
  not Ready" to go `Pending`. These two read "not Ready" only because the old
  controller said that for a failed image too. Their images really are
  `Failed`, so #685 keeps them `Failed`, and the message now says so.
  - The controller wrote each once (resourceVersion bumped at 06:30:47).
  - Both logged `"msg"="resolution failed"`.
- **No launcher pod appeared**, so the stop condition does not apply.
- **No `ResolutionFailed` event.** Neither guest has one. There are no events
  at all in `default` or `val-n2`. They were already `Failed` before the
  upgrade, so the event, which by #685's description comes with the
  transition, did not fire. R6-B checks the event on a guest that goes
  `Failed` under the new controller.

## sov: PASS

- **Upgrade:** Helm **36 → 37** at 06:29:53, rolled out by 06:30:17.
- **Images:** all `sha-4f08e87`, `--metrics-secure=false`, the same 4 VAPs.
- **Controller:** 13 "Starting workers", 0 error lines.
- **Events RBAC (SAR):** `allowed`.

## Baselines

| Guest | After the upgrade |
|---|---|
| dev `gpu-cells/innercp` | launcher uid `10bd28b8-…`, 0 restarts, Running (disk boot) |
| dev `field-testing/ft-gpu-pool-slot-j74r2` | uid `6aeee96c-…`, 0 restarts, Running, still holding the GPU |
| ntx `capi-udn/ks-udn-cp-54klw` | launcher uid `a5420720-…`, 0 restarts, Running; resourceVersion unchanged (`phase3-r6.md`) |
