# Phase 1 r7: upgrade to the candidate `8afbad3`

Run 2026-09-28, 08:30–08:34 UTC. Node names are generalised as in `phase0.md`.

| Cluster | Verdict |
|---|---|
| dev | **PASS**: Helm 49 → 50; every Deployment and DaemonSet rolled out |
| ntx | **PASS**: Helm 31 → 32; the two leftovers stay `Failed`, with no launcher pod |
| sov | **PASS**: Helm 37 → 38 |

## Build check and scope

- **Build check.** At 08:30:41 the chart `0.0.0-dev.8afbad3` and all nine
  `sha-8afbad3` images were present. `Release Dev`, `CI`, `Manifests`,
  `Security` and `SAST` for `8afbad3` all finished `success`.
- **Scope.** `git diff 4f08e87 8afbad3` touches no CRD, RBAC, Rust or chart
  template. It touches only `internal/controller/swiftmigration/dst_not_ready.go`,
  its test, and `docs/migration/troubleshooting.md`.
- **Space.** cp-1 at 08:30:48: 112.4 GiB free (57.4%).

## Pre-checks (08:31, and pre-check 1 again right before each upgrade)

| | dev | ntx | sov |
|---|---|---|---|
| Stopped-but-Running | none | none | none |
| `Failed` guests (pre-check 2) | none | `default/sample`, `val-n2/snapshot-local-source`: "SwiftImage failed: import job failed", 0 launcher pods | none |
| Guests | `gpu-cells/innercp`, `val-r6-dnr/r6d` (kept from R6-D): both `Running` | the two CAPI guests `Running`, and the two leftovers | none |
| Terminating namespaces | none | none | none |

## Commands (dev, then ntx, then sov)

- **CRDs.** Applied from a worktree at `8afbad3`: 15 `serverside-applied`.
  They are identical to `4f08e87`'s.
- **Helm.** `helm upgrade … --version 0.0.0-dev.8afbad3 --reuse-values`, with
  all nine `--set <c>.image.tag=sha-8afbad3`.
  - A `--dry-run` first exited 0 everywhere, with no `sha-4f08e87` left.
- **Rollout.** `rollout status` for every Deployment and DaemonSet:
  - dev: 08:31:34 → 08:32:00;
  - ntx: 08:32:04 → 08:32:23;
  - sov: 08:32:28 → 08:32:51.

## After the upgrade

| | dev | ntx | sov |
|---|---|---|---|
| Images | all `sha-8afbad3` (UI `v0.12.4`) | all `sha-8afbad3` | all `sha-8afbad3` |
| Controller | 13 "Starting workers", 0 errors, **`--metrics-secure=true`** | 13, 0 errors, `false` | 13, 0 errors, `false` |
| VAPs | the same 4 | the same 3 | the same 4 |
| SwiftKernels | 4 × `Ready` | `ft-faas` `Ready` | none |
| `Failed` guests | none | the two leftovers, unchanged ("SwiftImage failed: import job failed"), **0 launcher pods** | none |

## Baselines

| Guest | After the upgrade |
|---|---|
| dev `gpu-cells/innercp` | launcher uid `10bd28b8-…`, 0 restarts, Running |
| dev `field-testing/ft-gpu-pool-slot-j74r2` | uid `6aeee96c-…`, 0 restarts, Running; the GPU `0000:01:00.0` still allocated to it |
| dev `val-r6-dnr/r6d` (R6-D's kept guest) | launcher uid `26973c1b-…`, 0 restarts, Running (launcher still `sha-4f08e87`; no Rust change) |
| ntx `capi-udn/ks-udn-cp-54klw` | launcher uid `a5420720-…`, 0 restarts, Running; resourceVersion unchanged (`phase3-r7.md`) |

The 3 s watchers on dev and ntx saw no change in any guest, kernel, pool, slot
or GPU allocation during the upgrade.
