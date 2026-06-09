# Layer 3 — workloads

`workloads/<env>/` holds the environment's guests and pools. Unlike the infra
layer, it is **environment-specific by design** — staging and production run
different fleets at different sizes.

Working examples: [`examples/gitops-flux/workloads/`](../../examples/gitops-flux/workloads/)
(production: 3-replica `web` pool + a `db` guest; staging: 1-replica pool + a
test guest).

## Patterns

- **Scaling is a Git commit**: change `spec.replicas` on the pool. Pool rolling
  updates trigger on template **spec** changes only (the template hash is
  spec-only — metadata edits don't roll the fleet).
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

## Day-2 operations that stay imperative

Migration, snapshot/restore, console/SSH, and drain are **not** Git-managed —
they're operational verbs (`swiftctl migrate/snapshot/console`, `kubectl
drain`). They don't modify Git-managed specs (except as noted for nodeName),
so they coexist with Flux cleanly.
