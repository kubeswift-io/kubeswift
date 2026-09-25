# Phase 1 r2: upgrade to the candidate `7a1a76d`

Run 2026-09-25, 07:02–07:23 UTC. **PASS on all three clusters** (dev, ntx, sov).
Node names are generalised as in `phase0.md`.

## Build check

- **07:02:08** first check: the chart and 8 of 9 images were missing (only
  `controller-manager` was there).
- **07:07:54 / 07:13:12:** the chart, `swiftletd`, `migration-stunnel` and
  `kubeswift-gateway`, then the chart alone.
- **07:18:30:** `missing: none`.

The check used `helm show chart … --version 0.0.0-dev.7a1a76d` and
`docker manifest inspect …:sha-7a1a76d` for all nine images.

## Housekeeping before Phase 1 (dev)

- **`val-d8/mig16b` needed `swiftctl stop`, not the plain patch.** At 07:02:32
  I patched `spec.runPolicy: Stopped`. The launcher was still `Running` 5 min
  later, with no deletion, events or log lines.
  - This is **by design**. `internal/actions/actions.go` `Stop()` and
    `swiftctl stop --help` both say a runPolicy patch alone leaves the guest
    running; stopping is the patch **plus** deleting the launcher pod.
  - So I ran `swiftctl stop mig16b -n val-d8` (the same `Stop()` action), at
    07:08:55. The launcher was gone at 07:09:02, and `mig16b` is `Stopped`.
  - Note for future plans: "set runPolicy Stopped and wait for the pod to go"
    will not complete by itself.
- **Left as they were:** `mig16`, the four SwiftMigrations and the `val-d8`
  namespace; `val-d2` and `val-d3` (still Terminating); the ntx leftovers.

## Commands (dev, then ntx, then sov)

- A worktree at `7a1a76d`. `git diff --stat d232581 7a1a76d -- charts/kubeswift/crds/`
  is empty, so no CRD changed; they were applied anyway.
- `kubectl apply --server-side --force-conflicts -f <wt>/charts/kubeswift/crds/`:
  all 15 `serverside-applied` on each cluster, and
  `swiftsnapshots status.guestSpec.primaryIP: string` is still present.
- `helm upgrade … --version 0.0.0-dev.7a1a76d --reuse-values --dry-run` exited 0
  on each cluster, and was then run for real with `--set <c>.image.tag=sha-7a1a76d`
  for all nine components (`ui` excepted).
- `kubectl rollout status` for every kubeswift Deployment and DaemonSet.

## Results

| | dev | ntx | sov |
|---|---|---|---|
| Helm revision | 43 → **44** | 26 → **27** | 32 → **33** |
| Chart / app | `0.0.0-dev.7a1a76d` | same | same |
| Upgrade → rollouts done | 07:19:05 → 07:20:39 | 07:21:25 → 07:21:45 | 07:22:44 → 07:23:07 |
| Images | controller-manager, kubeswift-gateway, gpu-discovery, kubeswift-dra-driver, and the env images swiftletd, snapshot-s3, snapshot-oras, migration-stunnel, sandbox-materialize: all `sha-7a1a76d`; `kubeswift-ui:v0.12.4` unchanged | controller-manager + the 5 env images: `sha-7a1a76d` | same as ntx |
| `--metrics-secure` | **false** | **false** | **false** |
| Controllers started | 13, no new error lines (the only errors are the known `val-d2`/`val-d3` snapshot-finalizer retries) | 13; all 11 error lines are the same finalizer retries, for `val-n2r` (see `phase3-r2.md`) | 13, 0 errors |
| VAPs (all `[Deny]`) | gateway-exec-gate, launcher-sa-gate, launcher-sa-token-secret-gate, launcher-sa-tokenrequest-gate | launcher-sa-gate, launcher-sa-token-secret-gate, launcher-sa-tokenrequest-gate (no exec gate: standalone) | the same 4 as dev |
| `kubeswift-gateway-console` | present, bound to `kubeswift-system/kubeswift-gateway` | – | present, bound to the gateway SA |
| `kubeswift-vm-reader` `pods/exec` rules | 0 | – | 0 (has `swiftguests/console`, `swiftsandboxes/exec`, `swiftsandboxes/log`) |
| Federation (dev) | `kubeswift` and `sov` both READY | – | – |

## Running-guest baselines: not disturbed

| Guest | Before | After |
|---|---|---|
| dev `gpu-cells/innercp` | uid `10bd28b8-…`, 0 restarts, started 2026-09-16T07:46:36Z, `swiftletd:v0.13.15` | **same UID, 0 restarts**, Running |
| ntx `capi-udn/ks-udn-cp-54klw` | uid `a5420720-…`, 0 restarts, started 2026-09-21T21:59:01Z, `swiftletd:v0.13.14`, SwiftGuest rv `31535194` | **same UID, 0 restarts**, rv still `31535194`, every condition timestamp identical |

Existing launchers keep the image they were created with. The launcher-side
fixes (#663's swiftletd half and #665) reach only guests created after this
upgrade, which is how Phase 2 r2 uses them.
