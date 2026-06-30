# GitOps with KubeSwift — overview

KubeSwift is Kubernetes-native end to end — every operational surface is a CRD
— so it composes with GitOps tooling without adapters: your Git repository
declares the platform, the infrastructure resources, and the VM fleets, and a
reconciler (FluxCD in our reference; Argo CD works the same way) keeps the
cluster converged to it.

## The three-layer model

| Layer | What | Git objects | Changes |
|---|---|---|---|
| **1 — Platform** | KubeSwift itself: controllers, webhooks, **CRDs** | `OCIRepository` + `HelmRelease` for `oci://ghcr.io/kubeswift-io/charts/kubeswift` | rarely (version bumps) |
| **2 — Infrastructure** | SwiftGuestClass, SwiftImage, SwiftSeedProfile, SwiftGPUProfile, NADs | plain manifests under `infrastructure/` | occasionally |
| **3 — Workloads** | SwiftGuest, SwiftGuestPool | per-environment manifests under `workloads/<env>/` | often |

Layers are wired with Flux `dependsOn` so apply order is **platform (CRDs) →
infra → workloads**. Two KubeSwift-specific nuances make this ordering safe and
fast:

- **CRDs ship with the chart and MUST be upgraded with it** (`upgrade.crds:
  CreateReplace` on the HelmRelease). The apiserver *silently drops* fields a
  stale CRD schema doesn't know — the most insidious upgrade failure mode
  KubeSwift has (the "stale-CRD-silent-strip": a new controller writes a new
  status/spec field, the old CRD strips it, and everything looks fine while
  being subtly broken).
- **Image imports are asynchronous.** A SwiftImage takes minutes to download,
  convert, and resize. Set `wait: false` on the infra Kustomization: workloads
  apply immediately and SwiftGuests sit in `Pending` until their image is
  `Ready` — the controller handles the wait gracefully.

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

Reference layout + working manifests: [`examples/gitops-flux/`](../../examples/gitops-flux/).
