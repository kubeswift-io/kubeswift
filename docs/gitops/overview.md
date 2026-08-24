# GitOps with KubeSwift — overview

KubeSwift is Kubernetes-native end to end — every operational surface is a CRD
— so it composes with GitOps tooling without adapters: your Git repository
declares the platform, the infrastructure resources, and the VM fleets, and a
reconciler (FluxCD in our reference; Argo CD works the same way) keeps the
cluster converged to it.

## The three-layer model

| Layer | What | Git objects | Changes |
|---|---|---|---|
| **1 — Platform** | KubeSwift itself: controllers, webhooks, admission policy, **CRDs** | `OCIRepository` + `HelmRelease` for `oci://ghcr.io/kubeswift-io/charts/kubeswift` | rarely (version bumps) |
| **2 — Infrastructure** | SwiftGuestClass, SwiftImage, SwiftSeedProfile, SwiftKernel, SwiftGPUProfile, NADs | plain manifests under `infrastructure/` | occasionally |
| **3 — Workloads** | SwiftGuest, SwiftGuestPool, SwiftSandboxPool, SwiftSnapshotSchedule | per-environment manifests under `workloads/<env>/` | often |

Layers are wired with Flux `dependsOn` so apply order is **platform (CRDs) →
infra → workloads**. Two KubeSwift-specific nuances make this ordering safe and
fast:

- **CRDs ship with the chart and MUST be upgraded with it** (`upgrade.crds:
  CreateReplace` on the HelmRelease). The apiserver *silently drops* fields a
  stale CRD schema doesn't know — the most insidious upgrade failure mode
  KubeSwift has (the "stale-CRD-silent-strip": a new controller writes a new
  status/spec field, the old CRD strips it, and everything looks fine while
  being subtly broken). **This is the strongest single argument for driving
  KubeSwift with GitOps**: `helm upgrade` on the CLI cannot upgrade CRDs at all
  (Helm treats `crds/` as install-only, with no flag to change it), so every
  CLI upgrade must be followed by a manual `kubectl apply -f
  charts/kubeswift/crds/`. Flux's helm-controller applies CRDs itself, so
  `upgrade.crds: CreateReplace` closes the hole the CLI leaves open.
- **Image imports are asynchronous.** A SwiftImage takes minutes to download,
  convert, and resize. Set `wait: false` on the infra Kustomization: workloads
  apply immediately and SwiftGuests sit in `Pending` until their image is
  `Ready` — the controller handles the wait gracefully.

## What does NOT belong in Git

Roughly half of KubeSwift's fifteen CRDs are not declarative desired state, and
committing them causes real problems rather than merely being untidy:

| Kind | Why not | What happens if you do |
|---|---|---|
| **SwiftSandbox** | Ephemeral. It runs a workload once and stops at `Completed`/`Failed` | With `spec.ttl` set, the controller deletes it and Flux recreates it on the next reconcile — a sandbox that reruns on a loop instead of once. Without `ttl` it instead sits terminal forever, and since re-running means deleting it, Git can never express "run this again" either way |
| **SwiftSnapshot**, **SwiftRestore**, **SwiftMigration** | One-shot imperative verbs, terminal once complete | Same recreate loop: a snapshot every reconcile interval, or a migration re-fired after it succeeded |
| **SwiftGPUNode** | Discovery-owned. `spec` is intentionally empty and `status` is written by the discovery DaemonSet | Nothing useful; you are committing an object whose whole content the cluster owns |
| **fleet `Cluster`** for the local cluster | The chart self-registers it when `federation.role=hub` and `federation.selfRegister.enabled` | Two managers of one object. Git-manage *remote* fleet members if you like; leave the self-registered local one to the chart |

Schedules are the declarative counterpart of one-shot verbs, and those *do*
belong in Git: commit a `SwiftSnapshotSchedule` rather than a stream of
`SwiftSnapshot` objects.

## When to use it

GitOps fits KubeSwift best when you have more than one cluster or environment,
more than one person changing fleets, or compliance needs (the Git history *is*
the audit trail of every VM-fleet change). For a single dev cluster,
`make deploy-with-webhook` + `kubectl apply` remains the faster loop.

## Trade-offs to know upfront

- **Prune vs VM lifecycle.** `prune: true` on the workloads Kustomization means
  deleting a guest's manifest from Git **deletes the VM**. That is the point of
  GitOps — but a mis-merge can delete a fleet. Protect with Git review rules,
  and consider `prune: false` for stateful one-offs like the `db` guest.
- **Drift is corrected, not reported.** Manual `kubectl edit` changes to
  Git-managed CRs are reverted on the next reconcile (default 10m). Imperative
  day-2 verbs that *don't* edit Git-managed specs remain fine: `swiftctl
  migrate`, snapshots/restores, `swiftctl stop` (note: a runPolicy flip done
  via `kubectl` will be reverted; change it in Git).
- **Secrets must not live in seed user-data in Git.** See
  [secrets.md](secrets.md).
- **Migrations move guests, Git doesn't know.** `spec.nodeName` written by the
  migration controller is a *spec* change on a Git-managed object — keep
  `nodeName` out of Git-managed guest specs (let the scheduler/migration own
  placement), or Flux will fight the migration controller.
- **Scale subresources and Git are two writers.** `SwiftGuestPool.spec.replicas`
  and `SwiftSandboxPool.spec.minWarm` both back a `scale` subresource, so
  `kubectl scale` and any HPA-style autoscaler write the same field Git owns.
  Pick one: scale from Git, or drop the field from the Git manifest and let the
  autoscaler own it. Doing both produces a pool that oscillates on the
  reconcile interval.
- **Some fields are immutable once a resource starts working.** SwiftImage in
  particular rejects spec edits after import begins, so a Kustomization can go
  `Ready=False` on a change that looks harmless in review. See
  [infrastructure.md](infrastructure.md).

Reference layout + working manifests: [`examples/gitops-flux/`](../../examples/gitops-flux/).
