# Layer 3 — workloads

`workloads/<env>/` holds the environment's guests, pools and schedules. Unlike
the infra layer, it is **environment-specific by design** — staging and
production run different fleets at different sizes.

Working examples: [`examples/gitops-flux/workloads/`](../../examples/gitops-flux/workloads/)
(production: 3-replica `web` pool + a `db` guest; staging: 1-replica pool + a
test guest).

## What belongs here

| Kind | Notes |
|---|---|
| `SwiftGuest` | a long-lived VM |
| `SwiftGuestPool` | a fleet; `spec.replicas` backs a `scale` subresource |
| `SwiftSandboxPool` | warm pre-booted sandbox slots; `spec.minWarm` backs a `scale` subresource |
| `SwiftSnapshotSchedule` | cron snapshots with keep-N retention |

`SwiftSandbox` does **not** belong here, and neither do `SwiftSnapshot`,
`SwiftRestore` or `SwiftMigration`. They are one-shot and terminal, so Git
management turns them into a loop — see [overview.md](overview.md).

## Patterns

- **Scaling is a Git commit**: change `spec.replicas` on the pool. Pool rolling
  updates trigger on template **spec** changes only (the template hash is
  spec-only — metadata edits don't roll the fleet).
- **Do not let an autoscaler and Git both own a scale field.** Both pool kinds
  expose `scale`, so `kubectl scale`, an HPA or a KEDA ScaledObject write the
  same field the manifest declares. Either scale from Git, or omit the field
  from the manifest and let the autoscaler own it. Both at once gives you a pool
  that flaps on the reconcile interval, and the Kustomization looks healthy the
  whole time.
- **Separate dirs vs overlays**: the reference uses separate per-env dirs
  (simplest, fleets genuinely differ). If your environments converge, use a
  shared base + Kustomize overlays patching `replicas`/`imageRef` per env —
  both work; pick per-fleet.
- **Keep `nodeName` out of Git-managed guests.** Placement belongs to the
  scheduler and the migration controller; a Git-pinned `nodeName` makes Flux
  fight `swiftctl migrate` (see [overview.md](overview.md) trade-offs).
- **Stateful one-off guests** (databases): consider `prune: false` on a
  dedicated Kustomization so a mis-merge can't delete them, and prefer
  `runPolicy: RestartOnFailure`.

## Sandbox pools

A `SwiftSandboxPool` keeps `minWarm` pre-booted slots so a sandbox checkout is
sub-second instead of a cold boot. It is ordinary declarative state and belongs
in Git. Two things to know:

- **A warm slot is a Pod, not a SwiftSandbox.** You will not see custom
  resources for the slots, and you should not try to declare them.
- **GPU is the slot's shape.** A pool with `spec.gpuProfileRef` pre-boots slots
  each holding a GPU, so `minWarm` should stay at or below the number of free
  GPUs — a pool asking for more warm GPU slots than the cluster has will sit
  partially unfilled. The consuming `SwiftSandbox` sets `poolRef` only and
  inherits the GPU; it never asks for one itself.

## Snapshot schedules

`SwiftSnapshotSchedule` is the declarative form of backup, and the right thing
to commit:

```yaml
apiVersion: snapshot.kubeswift.io/v1alpha1
kind: SwiftSnapshotSchedule
metadata: { name: nightly-db }
spec:
  schedule: "0 2 * * *"          # 5-field cron, UTC
  concurrencyPolicy: Forbid      # skip a tick while a prior capture runs
  retention:
    keepLast: 7                  # note: under retention, not at spec level
  template:
    spec:
      guestRef: { name: db }
      backend: { type: csi-volume-snapshot }
```

More variants in [`config/samples/snapshot-schedule/`](../../config/samples/snapshot-schedule/),
and a second schedule exporting to an OCI registry in
[`examples/gitops-flux/workloads/production/db-snapshot-schedules.yaml`](../../examples/gitops-flux/workloads/production/db-snapshot-schedules.yaml).
An OCI export is the portable one: a CSI snapshot is trapped in the cluster
that took it. See [oci-artifacts.md](oci-artifacts.md).

`suspend: true` is the GitOps-friendly way to pause backups: a reviewed commit
rather than a `kubectl` edit that the next reconcile reverts. Retention is
enforced by the controller, so the SwiftSnapshot objects it creates and deletes
must stay out of Git.

## Day-2 operations that stay imperative

Migration, snapshot/restore, console/SSH, sandbox exec, and drain are **not**
Git-managed — they're operational verbs (`swiftctl migrate/snapshot/console`,
`kubectl drain`). They don't modify Git-managed specs (except as noted for
nodeName), so they coexist with Flux cleanly.
