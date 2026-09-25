# Phase 1 r5: upgrade to the candidate `08d0165`

Run 2026-09-25, 23:48–23:56 UTC. Node names are generalised as in `phase0.md`.

| Cluster | Verdict |
|---|---|
| dev | **PASS**: Helm 47 → 48; every Deployment and DaemonSet rolled out in 24 s; `ft-gpu-pool`'s failed slot was deleted with a `SlotEnded` event, and the GPU is free |
| ntx | **PASS**: Helm 29 → 30 |
| sov | **PASS**: Helm 35 → 36 |

## Build check

At 23:48:38 the chart `0.0.0-dev.08d0165` and all nine `sha-08d0165` images
were present. `Release Dev`, `CI`, `Security` and `SAST` for `08d0165` had
finished `success` at 23:00.

## Pre-check (runPolicy), on every cluster

**Stopped-but-Running guests: none on any cluster.** Checked at 23:48:55 and
again right before each upgrade (dev 23:50:15, ntx 23:51:36, sov 23:52:04).

| | dev | ntx | sov |
|---|---|---|---|
| Guests (`runPolicy` / phase / boot) | `gpu-cells/innercp` Always / Running / disk | `capi-udn/ks-udn-cp-54klw` (unset) / Running / disk; `capi-udn/ks-udn-md0-…` (unset) / Running / disk; `default/sample` Running / Failed and `val-n2/snapshot-local-source` Running / Failed (round-1 leftovers, unchanged) | none |
| `kubeswift-system` labels | `kubernetes.io/metadata.name`, `name` | same | same |
| SwiftKernels | 4, all Ready | `field-testing/ft-faas` Ready | none |
| Terminating namespaces | none | none | none |
| `can-i list events` (controller SA) | `no` | `no` | `no` |

## Commands (dev, then ntx, then sov)

- **CRDs.** Applied from a worktree at `08d0165`: 15 `serverside-applied`, 0
  errors.
  - `charts/kubeswift/crds/` is identical to `9604992`'s, as the go-ahead says.
- **Helm.** `helm upgrade kubeswift oci://ghcr.io/kubeswift-io/charts/kubeswift
  --version 0.0.0-dev.08d0165 --reuse-values`, with `--set <c>.image.tag=sha-08d0165`
  for all nine components.
  - A `--dry-run` first exited 0 everywhere, with no `sha-9604992` left in
    the render.
  - The rendered controller ClusterRole has `events: create, patch, list`.
- **Rollout.** `kubectl rollout status` for every kubeswift Deployment and
  DaemonSet.

## dev: PASS

- **Upgrade:** Helm **47 → 48** at 23:50:15.
  - `controller-manager`, `kubeswift-gateway`, `kubeswift-ui`, `gpu-discovery`
    and `kubeswift-dra-driver` had all rolled out by 23:50:39.
  - The Multus change from round 4 held: no sandbox errors this time.
- **Images:** all nine on `sha-08d0165`, including the controller's
  `KUBESWIFT_*_IMAGE` env vars. The UI stays on `v0.12.4`.
- **Controller:**
  - 13 "Starting workers", 0 error lines;
  - **`--metrics-secure=true` survived** (`"Serving metrics server" … "secure"=true`);
  - VAPs: the same 4, all `[Deny]`.
- **Events RBAC (#682):** `can-i list events` → **`yes`**.
- **SwiftKernels:** all 4 stayed `Ready`, and nothing re-pulled. That is
  expected: the kernel path did not change this time.
- **Baseline:** `innercp`'s launcher has the same uid `10bd28b8-…`, 0 restarts,
  `Running` throughout.

### `field-testing/ft-gpu-pool` (#683), observed only

Before the upgrade:
- slot pod `ft-gpu-pool-slot-dfjtk` `Failed` since 2026-09-24 21:41;
- the GPU `allocated: true, allocatedTo: sandbox:field-testing/ft-gpu-pool-slot-dfjtk`;
- the pool `Degraded`, because `alpine:3` hits the Docker Hub pull rate limit.

A 3 s watcher after the upgrade:

```text
23:50:07 slots: field-testing/ft-gpu-pool-slot-dfjtk:Failed   gpu: 0000:01:00.0 allocated=true to=sandbox:field-testing/ft-gpu-pool-slot-dfjtk
23:50:58 slots: (none)
23:50:58 pool event: Warning SlotEnded x1: "warm slot ft-gpu-pool-slot-dfjtk ended (Failed): launcher pod failed; deleted it"
23:50:58 gpu: 0000:01:00.0 allocated=false to=-
23:55:50 (unchanged) pool Degraded, Resolved=False/ImageResolveFailed (Docker Hub rate limit), Warm=False; still 1 SlotEnded event
```

- **The slot is deleted and the GPU freed**, 43 s after the new controller
  started.
- **The event names the slot and its phase (`Failed`).** Its message is only
  "launcher pod failed". It does not carry the pod's own failure (the round-4
  cause was `Cannot open initramfs file`).
- **No replacement slot was created**, because the pool cannot resolve its
  image. So there is no delete-and-recreate loop.
- The pool keeps retrying the image resolve. If the rate limit clears, it may
  warm a slot that takes the GPU again.

## ntx: PASS

- **Upgrade:** Helm **29 → 30** at 23:51:39, rolled out by 23:52:04.
- **Images:** all `sha-08d0165`, `--metrics-secure=false`, VAPs as before (3).
- **Controller:** 13 "Starting workers", 0 error lines.
- **Events RBAC:** `can-i list events` → **`yes`**.
- `field-testing/ft-faas` stayed `Ready`.
- N3 is in `phase3-r5.md`.

## sov: PASS

- **Upgrade:** Helm **35 → 36** at 23:52:09, rolled out by 23:52:34.
- **Images:** all `sha-08d0165`, `--metrics-secure=false`, the same 4 VAPs.
- **Controller:** 13 "Starting workers", 0 error lines.
- **Events RBAC:** `can-i list events` → **`yes`**.
- S1 is in `phase3-r5.md`.

## Events RBAC, confirmed directly

A `SubjectAccessReview` for `system:serviceaccount:kubeswift-system:controller-manager`,
`list events`, cluster-wide:

```text
dev  allowed=true  RBAC: allowed by ClusterRoleBinding "kubeswift-controller-manager" of ClusterRole "kubeswift-controller-manager"
ntx  allowed=true  (same)
sov  allowed=true  (same)
```

## Baselines

| Guest | After the upgrade |
|---|---|
| dev `gpu-cells/innercp` | launcher uid `10bd28b8-…`, 0 restarts, Running (disk boot) |
| ntx `capi-udn/ks-udn-cp-54klw` | launcher uid `a5420720-…`, 0 restarts, Running (disk boot); resourceVersion unchanged (see `phase3-r5.md`) |
