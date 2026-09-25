# Phase 1 r3: upgrade to the candidate `f260277`

Run 2026-09-25, 10:54–11:09 UTC. **PASS on all three clusters.**
Node names are generalised as in `phase0.md`.

## Build check

| Time (UTC) | Missing |
|---|---|
| 10:54:34 | chart + 7 images |
| 10:59:49 | chart, `swiftletd`, `migration-stunnel`, `kubeswift-gateway` |
| 11:05:08 | **none** |

Checked with `helm show chart … --version 0.0.0-dev.f260277` and
`docker manifest inspect …:sha-f260277` for all nine images.

## Housekeeping (dev)

- **`val-r2` and `val-d8` are deleted.** I issued
  `kubectl delete ns val-r2 val-d8` at 10:54:26, and both were gone by
  10:55:04. They held no snapshots, and neither stuck.
  - Before deletion, `val-r2` held `r4g` (Running) and its 7 SwiftMigrations.
  - `val-d8` held `mig16`/`mig16b` (Stopped) and 4 SwiftMigrations.
- **Kept:** the cluster-scoped class `val-migratable-16g`.
- **Untouched:** `val-d2`, `val-d3` (still Terminating).

## Commands (dev, then ntx, then sov)

- A worktree at `f260277`. `git diff --stat 7a1a76d f260277 -- charts/kubeswift/crds/`
  is empty; the CRDs were applied anyway (all 15 `serverside-applied`).
- `helm upgrade kubeswift oci://ghcr.io/kubeswift-io/charts/kubeswift --version
  0.0.0-dev.f260277 -n kubeswift-system --reuse-values`, with
  `--set <c>.image.tag=sha-f260277` for all nine components. A `--dry-run`
  first exited 0 on each cluster.
- `kubectl rollout status` for every kubeswift Deployment and DaemonSet.

## Results

| | dev | ntx | sov |
|---|---|---|---|
| Helm revision | 45 → **46** | 27 → **28** | 33 → **34** |
| Chart / app | `0.0.0-dev.f260277` | same | same |
| Upgrade → rollouts done | 11:05:34 → 11:07:13 | 11:07:39 → 11:07:58 | 11:08:26 → 11:08:48 |
| Images | controller-manager, kubeswift-gateway, gpu-discovery, kubeswift-dra-driver, and the env images swiftletd, snapshot-s3, snapshot-oras, migration-stunnel, sandbox-materialize: all `sha-f260277`; `kubeswift-ui:v0.12.4` unchanged | controller-manager + the 5 env images: `sha-f260277` | same as ntx |
| `--metrics-secure` | **true**, kept by `--reuse-values`: the log says `"Serving metrics server" … "secure"=true`, and `kubeswift-metrics-reader` is still bound to `monitoring/monitoring-kube-prometheus-prometheus` | false | false |
| Controllers | 13 started, 0 error lines apart from the known `val-d2`/`val-d3` finalizer retries | 13, 0 errors (apart from the known `val-n2r` retries) | 13, 0 errors |
| VAPs (all `[Deny]`) | gateway-exec-gate, launcher-sa-gate, launcher-sa-token-secret-gate, launcher-sa-tokenrequest-gate | launcher-sa-gate, launcher-sa-token-secret-gate, launcher-sa-tokenrequest-gate | the same 4 as dev |
| Federation (dev) | `kubeswift` and `sov` both Ready | – | – |

## Running-guest baselines: not disturbed

| Guest | After the upgrade |
|---|---|
| dev `gpu-cells/innercp` | uid `10bd28b8-…`, **0 restarts**, started 2026-09-16T07:46:36Z, `swiftletd:v0.13.15` (unchanged) |
| ntx `capi-udn/ks-udn-cp-54klw` | uid `a5420720-…`, **0 restarts**, started 2026-09-21T21:59:01Z, SwiftGuest rv `31535194` (unchanged since before round 1) |
