# Phase 1: upgrade to the candidate `d232581`

Run 2026-09-25, 00:05–00:11 UTC. **PASS on all three clusters** (dev, ntx, sov).

The CRDs were applied from a worktree at `d232581`, and the chart was
`oci://ghcr.io/kubeswift-io/charts/kubeswift --version 0.0.0-dev.d232581`.

Node hostnames are generalised as in `phase0.md`.

## Build check

The first check, at 23:59:44 UTC, found the chart and three images missing
(`swiftletd`, `migration-stunnel`, `kubeswift-gateway`). The re-check at
**00:05:49 UTC** found `missing: none`. Checks used
`helm show chart … --version 0.0.0-dev.d232581` and
`docker manifest inspect ghcr.io/kubeswift-io/kubeswift/<img>:sha-d232581` for
all nine images.

**Note on the superseded `2d146eb` go-ahead.** Before `d232581` was issued, the
`2d146eb` CRDs had been applied to dev only, at 23:10:56 UTC. The `helm upgrade`
was never run. The product code and CRDs of `2d146eb` and `d232581` are identical
(`git diff --stat 2d146eb d232581` touches only `CHANGELOG.md` and
`test/migration/migration-test.sh`), and the `d232581` CRDs were re-applied below.

## Commands, per cluster (dev, then ntx, then sov)

```bash
kubectl apply --server-side --force-conflicts -f <worktree@d232581>/charts/kubeswift/crds/
helm upgrade kubeswift oci://ghcr.io/kubeswift-io/charts/kubeswift --version 0.0.0-dev.d232581 \
  -n kubeswift-system --reuse-values --dry-run          # render check first; exit 0 on all three
helm upgrade kubeswift oci://ghcr.io/kubeswift-io/charts/kubeswift --version 0.0.0-dev.d232581 \
  -n kubeswift-system --reuse-values \
  --set controllerManager.image.tag=sha-d232581 --set swiftletd.image.tag=sha-d232581 \
  --set sandboxMaterialize.image.tag=sha-d232581 --set snapshotORAS.image.tag=sha-d232581 \
  --set snapshotS3.image.tag=sha-d232581 --set migrationStunnel.image.tag=sha-d232581 \
  --set gpuDiscovery.image.tag=sha-d232581 --set dra.image.tag=sha-d232581 \
  --set gateway.image.tag=sha-d232581
kubectl rollout status -n kubeswift-system <every deployment and daemonset>
```

On every cluster, all 15 CRDs reported `serverside-applied`, and
`swiftsnapshots` `status.guestSpec.primaryIP` is now present
(`type: string`; before, it was `MISSING`).

## Results

| | dev | ntx | sov |
|---|---|---|---|
| CRDs applied | 00:06:06 | 00:07:43 | 00:09:26 |
| Helm revision | 42 → **43** | 25 → **26** | 31 → **32** |
| Chart / app | `kubeswift-0.0.0-dev.d232581` | same | same |
| Upgrade → rollouts done | 00:06:28 → 00:06:52 | 00:07:57 → 00:08:16 | 00:09:44 → 00:10:06 |
| Rollouts | controller-manager, kubeswift-gateway, kubeswift-ui, gpu-discovery (DS), kubeswift-dra-driver (DS): all "successfully rolled out" | controller-manager: rolled out | controller-manager: rolled out |
| Controller args | `--leader-elect --webhook-enabled=true --metrics-secure=false` | `--leader-elect --webhook-enabled=false --metrics-secure=false` | `--leader-elect --webhook-enabled=false --metrics-secure=false` |
| `--metrics-secure` | **false** (expected under `--reuse-values`) | **false** | **false** |
| Controller log after start | `informer cache synced`; 13 controllers "Starting workers"; 0 error lines | 13 controllers; 0 error lines | 13 controllers; 0 error lines |

### Images after the upgrade

All nine kubeswift images are on `sha-d232581` wherever the cluster uses them.
`ui` stayed on `v0.12.4`.

| Image | dev | ntx | sov |
|---|---|---|---|
| controller-manager | `sha-d232581` | `sha-d232581` | `sha-d232581` |
| kubeswift-gateway | `sha-d232581` | (not deployed) | (not deployed) |
| gpu-discovery (DS) | `sha-d232581` | (not deployed) | (not deployed) |
| kubeswift-dra-driver (DS) | `sha-d232581` | (not deployed) | (not deployed) |
| kubeswift-ui | `v0.12.4` (unchanged) | – | – |
| `KUBESWIFT_LAUNCHER_IMAGE` (swiftletd) | `sha-d232581` | `sha-d232581` | `sha-d232581` |
| `KUBESWIFT_SNAPSHOT_S3_IMAGE` | `sha-d232581` | `sha-d232581` | `sha-d232581` |
| `KUBESWIFT_SNAPSHOT_ORAS_IMAGE` | `sha-d232581` | `sha-d232581` | `sha-d232581` |
| `KUBESWIFT_MIGRATION_STUNNEL_IMAGE` | `sha-d232581` | `sha-d232581` | `sha-d232581` |
| `KUBESWIFT_SANDBOX_MATERIALIZE_IMAGE` | `sha-d232581` | `sha-d232581` | `sha-d232581` |

### VAPs (all bound `[Deny]`)

| | dev | ntx | sov | Expected |
|---|---|---|---|---|
| `kubeswift-launcher-sa-gate` | yes | yes | yes | yes |
| `kubeswift-launcher-sa-token-secret-gate` | yes | yes | yes | yes |
| `kubeswift-launcher-sa-tokenrequest-gate` | **new** | **new** | **new** | everywhere ✔ |
| `kubeswift-gateway-exec-gate` | **new** | – | **new** | dev and sov, not ntx ✔ |

### Console grant (dev and sov)

- **ClusterRole `kubeswift-gateway-console`:** present on both, bound by the
  ClusterRoleBinding `kubeswift-gateway-console` to
  `ServiceAccount:kubeswift-system/kubeswift-gateway`. Its rules:
  `get swiftguests`, `get swiftsandboxes`, `get pods`, `create pods/exec`. Before
  the upgrade it was absent on both.
- **sov `kubeswift-vm-reader`:** before the upgrade it had
  `{"resources":["pods/exec"],"verbs":["create"]}`. After, it has **no
  `pods/exec`**, and gains
  `create swiftguests/console`, `create swiftsandboxes/exec` and
  `get swiftsandboxes/log`. The rest is unchanged. A jq count of rules granting
  `pods/exec` gives `0`.
- **dev `kubeswift-vm-reader`** (hub, read-only form): no `pods/exec` before or after.

### Running guests were not disturbed

| Guest | Launcher pod UID | Restarts | Before | After |
|---|---|---|---|---|
| dev `gpu-cells/innercp` | `10bd28b8-af0d-406a-9c76-577712483188` | 0 → 0 | Running, started 2026-09-16T07:46:36Z, `swiftletd:v0.13.15` | **same UID, 0 restarts**, SwiftGuest Running, resourceVersion unchanged (27953501) |
| ntx `capi-udn/ks-udn-cp-54klw` | `a5420720-639b-4b4c-883f-ceede8794fb0` | 0 → 0 | Running, started 2026-09-21T21:59:01Z, `swiftletd:v0.13.14` | **same UID, 0 restarts**; SwiftGuest resourceVersion unchanged (31535194) and every condition's `lastTransitionTime` identical |

The ntx CAPI worker guest `ks-udn-md0-sz2ql-bz9zs` was still relaunching every
~5 min, as recorded in Phase 0 (pod `89b5edd6-…` created at 00:07:26Z, before the
upgrade, and still Pending on the Longhorn attach afterwards). Upgraded launchers
pick up the new swiftletd image only when their pod is recreated; this one's
pod predates the upgrade, so it still shows `swiftletd:v0.14.1`.

### Federation (dev)

After the gateway restart, both fleet `Cluster` CRs are READY: `kubeswift`
(self) and `sov`. There are no error lines in the gateway log.

## Verdict

- dev: **PASS**. Phase 2 is unblocked.
- ntx: **PASS**. Phase 3 (N1–N3) is unblocked.
- sov: **PASS**. Phase 3 (S1) is unblocked.
