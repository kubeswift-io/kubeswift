# GitOps with FluxCD — Design and Work Plan

> **Status:** Design — not yet implemented
> **Audience:** KubeSwift maintainers, AI assistants picking up this work
> **Last updated:** April 25, 2026
> **Target file path in repo:** `docs/design/gitops-flux.md`

---

## Purpose

This document captures the architectural thinking and phased work plan for making
KubeSwift operable via GitOps using FluxCD. It is intentionally split into two parts:

1. **Concepts** — what we want and why, the architecture, design decisions
2. **Work Plan** — concrete, phased implementation tasks ready to execute

When picking this up, read the Concepts section in full first. The Work Plan
references concepts and assumes that context.

---

# Part 1 — Concepts

## Goal

Enable KubeSwift operators to drive their entire VM platform — installation,
infrastructure, and workloads — through Git, using FluxCD as the reconciliation
engine. The Git repository becomes the single source of truth for the desired
state of the cluster's VM fleet.

## Why FluxCD

FluxCD aligns naturally with KubeSwift for several reasons:

- **OCI-native** — KubeSwift distributes its Helm chart via OCI
  (`oci://ghcr.io/kubeswift-io/charts/kubeswift`). Flux's `OCIRepository` source
  is the modern, recommended way to consume OCI-distributed Helm charts.
- **Declarative reconciliation** — KubeSwift controllers already converge to
  desired state on a reconciliation loop. Flux applies the same model at the
  Git-to-cluster boundary. The two systems compose cleanly.
- **CRD-based configuration** — every KubeSwift resource is a CRD. Flux's
  `Kustomization` API applies arbitrary CRDs without special handling.
- **Drift detection** — Flux can detect and revert out-of-band changes. This
  enforces "Git is the source of truth" without requiring KubeSwift-specific
  tooling.

## Three Layers of GitOps for KubeSwift

GitOps for KubeSwift breaks cleanly into three layers. They are often confused;
keeping them separate is essential for a clean design.

### Layer 1 — Platform Installation

**What:** KubeSwift itself (controller-manager, webhooks, CRDs, RBAC).

**How:** Flux `HelmRelease` referencing an `OCIRepository` that points to the
KubeSwift Helm chart in ghcr.io.

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: kubeswift
  namespace: kubeswift-system
spec:
  interval: 24h
  url: oci://ghcr.io/kubeswift-io/charts/kubeswift
  ref:
    semver: "0.2.x"
  layerSelector:
    mediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
    operation: copy
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: kubeswift
  namespace: kubeswift-system
spec:
  interval: 1h
  chartRef:
    kind: OCIRepository
    name: kubeswift
  install:
    crds: CreateReplace
  upgrade:
    crds: CreateReplace
    remediation:
      retries: 3
  driftDetection:
    mode: enabled
  values:
    # Helm values from charts/kubeswift/values.yaml
```

**Status:** Works today, no code changes required. The Helm chart is already
OCI-distributed and supports CRD lifecycle.

### Layer 2 — Infrastructure Resources

**What:** Shared resources that VMs reference: `SwiftGuestClass`, `SwiftImage`,
`SwiftKernel`, `SwiftSeedProfile`, `SwiftGPUProfile`,
`NetworkAttachmentDefinition`. These are usually managed by the platform team.

**How:** Flux `Kustomization` pointing at a directory in a GitRepository.

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: kubeswift-infra
  namespace: flux-system
spec:
  interval: 10m
  path: ./infrastructure/kubeswift
  prune: true
  sourceRef:
    kind: GitRepository
    name: platform-repo
  dependsOn:
    - name: kubeswift           # the HelmRelease must be ready first
```

**Lifecycle nuance:** Some KubeSwift resources have async lifecycles. A
`SwiftImage` triggers an import Job that takes minutes. Flux applies the
resource and moves on; the KubeSwift controller handles the import. Workloads
that reference an image stay in `Pending` until the image is `Ready`. This is
correct behavior — Flux should not block on long-running resource preparation.

**Status:** Works today, no code changes required.

### Layer 3 — Workloads

**What:** `SwiftGuest` (individual VMs) and `SwiftGuestPool` (VM fleets).

**How:** Flux `Kustomization` pointing at a directory of workload manifests.

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: kubeswift-workloads-production
  namespace: flux-system
spec:
  interval: 5m
  path: ./workloads/production
  prune: true
  sourceRef:
    kind: GitRepository
    name: platform-repo
  dependsOn:
    - name: kubeswift-infra
```

**Key GitOps patterns this enables:**

- **Scale via Git** — change `replicas: 8` to `replicas: 12` in a
  `SwiftGuestPool`, commit, push. Flux applies the change, the pool controller
  creates 4 new VMs.
- **Rolling update via Git** — change the pool template (e.g., new
  `imageRef` or kernel version), commit, push. Flux applies the updated pool,
  the controller's rolling update logic replaces VMs per `maxUnavailable` and
  `maxSurge`.
- **Drift detection** — manual `kubectl scale` or `kubectl edit` is reverted
  by Flux to match Git.
- **PR-based workflow** — fleet changes go through code review.

**Status:** Works today, no code changes required.

## Recommended Repository Structure

The pattern below cleanly separates platform installation, infrastructure, and
workloads, and supports multiple environments via Kustomize overlays.

```
fleet-repo/
├── clusters/
│   ├── production/
│   │   ├── flux-system/                    # bootstrap (Flux itself)
│   │   ├── kubeswift-platform.yaml         # OCIRepository + HelmRelease
│   │   ├── kubeswift-infra.yaml            # Kustomization → infrastructure/
│   │   └── kubeswift-workloads.yaml        # Kustomization → workloads/production/
│   └── staging/
│       ├── flux-system/
│       ├── kubeswift-platform.yaml
│       ├── kubeswift-infra.yaml
│       └── kubeswift-workloads.yaml        # → workloads/staging/
│
├── infrastructure/
│   └── kubeswift/
│       ├── kustomization.yaml
│       ├── classes/
│       │   ├── default.yaml                # SwiftGuestClass
│       │   ├── gpu-large.yaml
│       │   └── ci-runner.yaml
│       ├── images/
│       │   ├── ubuntu-noble.yaml           # SwiftImage
│       │   └── rocky9.yaml
│       ├── seeds/
│       │   ├── minimal.yaml                # SwiftSeedProfile
│       │   └── ci-runner.yaml
│       ├── gpu/
│       │   ├── a100-pcie-single.yaml       # SwiftGPUProfile
│       │   └── h200-hgx-4gpu.yaml
│       └── networking/
│           ├── storage-net.yaml            # NetworkAttachmentDefinition
│           └── vlan100.yaml
│
└── workloads/
    ├── production/
    │   ├── kustomization.yaml
    │   ├── inference-fleet/
    │   │   └── pool.yaml                   # SwiftGuestPool: 8 GPU VMs
    │   └── database/
    │       └── guest.yaml                  # SwiftGuest: single DB VM
    └── staging/
        ├── kustomization.yaml
        ├── inference-fleet/
        │   └── pool.yaml                   # 2 GPU VMs (smaller)
        └── test-vm/
            └── guest.yaml
```

**Why this layout:**

- `clusters/<env>/` is the per-cluster entry point Flux bootstraps from.
- `infrastructure/` is shared across environments by default; environment
  differences live in the per-cluster Kustomizations as patches.
- `workloads/<env>/` is environment-specific by design — staging and production
  have different fleets.
- Kustomize overlays handle environment-specific values (replica counts, sizes,
  image versions) without duplication.

## Dependency Ordering

`dependsOn` between Kustomizations enforces apply order:

```
HelmRelease (kubeswift)
    ↓ dependsOn
Kustomization (kubeswift-infra)
    ↓ dependsOn
Kustomization (kubeswift-workloads)
```

`dependsOn` waits for the previous Kustomization to be `Ready`, not for every
resource it created to be fully reconciled. This is fine because:

- KubeSwift CRDs must exist before SwiftGuestClass etc. can be applied
  (HelmRelease provides them).
- SwiftGuests can be applied before SwiftImages are `Ready`; the SwiftGuest
  controller handles the wait gracefully (guest stays in `Pending`).

So the ordering is: **CRDs ready → infra applied → workloads applied**, and
async resource preparation (image imports) overlaps with workload creation.

## Open Design Question — Standardized `Ready` Condition

Flux performs health checks on resources by reading their conditions. By
default, Flux understands the `Ready` condition. KubeSwift today uses
resource-specific conditions:

- `SwiftGuest`: `Resolved`, `PodScheduled`, `GuestRunning`, `GPUAllocated`,
  `RootDiskCloned`
- `SwiftImage`: phase-based (`Ready` is currently a phase, not a condition)
- `SwiftKernel`: `Ready`, `Failed`, `NoKernelNodes` (already has `Ready`)
- `SwiftGuestPool`: pool-specific conditions

For the cleanest Flux integration, all KubeSwift resources should expose a
standard `Ready` condition that summarizes overall health. This is additive —
existing conditions remain — and lets Flux assess Kustomization readiness
without custom health checks.

This is the only behavioral change recommended for KubeSwift itself, and it is
optional. Without it, operators can configure custom health checks in Flux
Kustomizations using `spec.healthChecks` and a custom check expression.

## What Stays the Same

- KubeSwift's CRDs, controllers, webhooks — unchanged
- The Helm chart's structure — unchanged
- OCI distribution — already aligned with Flux
- Reconciliation semantics — already aligned with Flux
- Existing CLI (`swiftctl`) — unaffected; operators using GitOps may bypass it
  for fleet changes but still use it for debugging and console access

## Trade-offs and Considerations

**Pro: Auditability** — every fleet change is a Git commit with author, time,
diff, and PR review. This is unavailable with imperative `kubectl`.

**Pro: Disaster recovery** — losing the cluster does not lose desired state.
Re-bootstrap Flux from Git, the cluster reconciles back to the declared fleet.

**Pro: Multi-cluster fleet management** — Flux is designed for fleet
operations. KubeSwift fleets across many clusters can be managed from a single
Git repo with cluster-specific overlays.

**Con: Async resource lifecycles add complexity** — operators need to
understand that "Flux applied the SwiftImage" does not mean "the image is
imported." Tooling and dashboards should make this clear.

**Con: SwiftGuest restart-via-recreate** — if a workflow requires recreating
a SwiftGuest (vs editing it), Flux's drift detection may fight that workflow.
The recommended pattern is to use SwiftGuestPool for fleets where
recreate-on-update is desired, and direct SwiftGuest for stable VMs.

**Con: Secrets management** — SwiftSeedProfile contains user-data which often
contains SSH keys or credentials. These should not live in plain Git. The
standard solution is SOPS or sealed-secrets, both of which integrate cleanly
with Flux. This is a documentation concern, not a KubeSwift code concern.

---

# Part 2 — Phased Work Plan

The work plan is structured to deliver value incrementally. Each phase is
independently shippable and provides operator-visible value. No phase requires
the next.

## Phase 0 — Validation and Reference Setup (no code, no docs yet)

**Goal:** Prove the three-layer model works end-to-end on a real cluster
before writing documentation that promises it does.

**Tasks:**

1. Set up a test cluster with Flux installed (`flux bootstrap`).
2. Create a private test Git repo with the recommended structure.
3. Layer 1: deploy KubeSwift via Flux HelmRelease (use `v0.2.0-rc.1` chart).
   - Verify CRD lifecycle works (`crds: CreateReplace`).
   - Verify drift detection reverts manual changes.
4. Layer 2: deploy infrastructure resources via Kustomization.
   - Apply a SwiftImage and verify it imports correctly under GitOps.
   - Apply a SwiftGuestClass, SwiftSeedProfile.
5. Layer 3: deploy a SwiftGuest and a SwiftGuestPool.
   - Verify scaling via Git commit (replicas 2 → 4 → 2).
   - Verify rolling update via Git commit (template change).
   - Verify drift detection reverts a manual `kubectl scale`.
6. Document any rough edges discovered.

**Deliverable:** A confirmed-working test repo and a list of issues (if any)
to address in later phases.

**Out of scope for this phase:** writing the user-facing documentation,
publishing examples, code changes.

## Phase 1 — Reference Repository (public examples)

**Goal:** Ship a public reference Git repository that operators can fork or
copy as a starting point.

**Tasks:**

1. Create `examples/gitops-flux/` in the KubeSwift repo (or a separate public
   repo `kubeswift-io/kubeswift-fleet-example`).
2. Implement the full directory structure from the Concepts section.
3. Include working examples for:
   - Layer 1: HelmRelease for KubeSwift platform install
   - Layer 2: at least 2 SwiftGuestClasses, 2 SwiftImages, 2 SwiftSeedProfiles,
     1 SwiftGPUProfile, 1 NetworkAttachmentDefinition
   - Layer 3: at least 1 SwiftGuest and 1 SwiftGuestPool
4. Multi-environment example with staging and production overlays using
   Kustomize.
5. Include a README explaining the structure and how to fork/adapt it.
6. Include CI validation in the example repo (kubeconform, kustomize build).

**Deliverable:** A reference repository operators can copy and adapt in under
an hour.

**Acceptance:**
- `kustomize build clusters/production` succeeds
- `flux-operator tree` (or equivalent) shows the expected hierarchy
- Smoke test: bootstrap Flux on a fresh cluster against the example repo,
  verify a SwiftGuestPool comes up

## Phase 2 — Documentation

**Goal:** Ship `docs/gitops/` covering the three-layer model, common patterns,
and operational guidance.

**Tasks:**

1. `docs/gitops/overview.md` — the three-layer model, when to use GitOps with
   KubeSwift, trade-offs.
2. `docs/gitops/flux-installation.md` — how to install Flux and bootstrap a
   repo (or link to upstream Flux docs).
3. `docs/gitops/platform-install.md` — Layer 1 deep dive.
4. `docs/gitops/infrastructure.md` — Layer 2 deep dive, including
   SwiftImage import lifecycle nuances.
5. `docs/gitops/workloads.md` — Layer 3 deep dive, scaling and rolling
   update patterns.
6. `docs/gitops/multi-environment.md` — Kustomize overlay patterns for
   staging/production.
7. `docs/gitops/secrets.md` — SOPS or sealed-secrets integration for
   SwiftSeedProfile user-data.
8. `docs/gitops/troubleshooting.md` — common issues (Kustomization stuck,
   drift loops, dependency ordering, image import delays).
9. Update README to link to `docs/gitops/`.

**Deliverable:** Comprehensive operator documentation with copy-pasteable
examples.

**Acceptance:**
- All examples in docs come from the Phase 1 reference repository
  (no doc-only snippets that haven't been tested)
- Internal review by at least one operator unfamiliar with the design

## Phase 3 — Standardize `Ready` Condition (optional code change)

**Goal:** Add a standard `Ready` condition to all KubeSwift resources for
clean Flux health check integration.

**Scope:** SwiftGuest, SwiftImage, SwiftKernel, SwiftGuestPool,
SwiftGPUProfile (where meaningful), SwiftGPUNode.

**Tasks:**

1. Audit each resource's existing conditions and decide what `Ready=True`
   means for each.
   - `SwiftGuest`: `Ready=True` when `phase=Running` and `GuestRunning=True`
   - `SwiftImage`: `Ready=True` when `phase=Ready`
   - `SwiftKernel`: already has `Ready` — verify semantics match
   - `SwiftGuestPool`: `Ready=True` when `availableReplicas == replicas`
   - `SwiftGPUNode`: `Ready=True` when `phase=Ready`
2. Update each controller to set the `Ready` condition alongside existing
   conditions. **Do not remove existing conditions** — additive only.
3. Update CRD OpenAPI schemas (`make generate`).
4. Update Helm chart CRDs (`cp config/crd/bases/*.yaml charts/kubeswift/crds/`).
5. Add unit tests for the new condition logic.
6. Update `swiftctl describe` to show the `Ready` condition prominently.
7. Update Phase 1 reference repo to use Flux native health checks
   (no custom expressions needed).

**Deliverable:** A KubeSwift release (v0.3.0 or later) where Flux health
checks work out of the box.

**Acceptance:**
- All existing tests pass
- New unit tests cover Ready transitions
- Flux Kustomizations targeting KubeSwift resources show correct ready/not-ready
  status without custom health check expressions

## Phase 4 — Notification Integration (optional code change)

**Goal:** Surface KubeSwift lifecycle events through Flux's notification
controller.

**Tasks:**

1. Audit existing Kubernetes Events emitted by KubeSwift controllers.
2. Add events for key transitions if missing:
   - SwiftImage import started/succeeded/failed
   - SwiftGuest VM started/stopped/failed
   - SwiftGuestPool rolling update started/completed/stuck
   - SwiftGPU allocation succeeded/failed
3. Document Flux Alert configurations targeting KubeSwift events for common
   notifications (Slack, Teams, generic webhook).
4. Add `docs/gitops/notifications.md`.

**Deliverable:** Operators can wire KubeSwift events into their existing
Flux notification pipeline with copy-pasteable configurations.

**Acceptance:**
- Sample Alert configurations validated against a real Flux installation
- Events appear in Flux's notification stream

## Phase 5 — Multi-Cluster Fleet Management (documentation + examples)

**Goal:** Document and demonstrate KubeSwift fleet management across many
clusters using Flux.

**Tasks:**

1. Extend the reference repo with a multi-cluster example
   (`clusters/prod-us-east/`, `clusters/prod-eu-west/`, etc.).
2. Document how to handle cluster-specific concerns (different GPU models per
   cluster, different network configurations).
3. Document how to share infrastructure resources (images, classes, profiles)
   across clusters with per-cluster overrides.
4. Cover Flux Operator's `ResourceSet` API for self-service workflows where
   multiple teams provision VMs in shared clusters.

**Deliverable:** Reference patterns for organizations running KubeSwift on
multiple clusters.

## Phase Sequencing and Dependencies

```
Phase 0 (validation)
    ↓
Phase 1 (reference repo) — depends on Phase 0
    ↓
Phase 2 (documentation) — depends on Phase 1
    ↓
Phase 3 (Ready condition)  ←  Phase 4 (notifications)  ←  Phase 5 (multi-cluster)
   (optional, parallel)        (optional, parallel)        (optional)
```

Phases 0–2 are the critical path. Phases 3–5 are independent enhancements that
can be picked up in any order based on operator demand.

## Estimated Effort

Rough sizing for a single contributor familiar with KubeSwift, Flux, and
Kustomize:

| Phase | Effort |
|-------|--------|
| 0 — Validation | 1–2 days |
| 1 — Reference repo | 2–3 days |
| 2 — Documentation | 2–3 days |
| 3 — Ready condition | 1–2 days |
| 4 — Notifications | 1 day |
| 5 — Multi-cluster | 1–2 days |

Total for the critical path (0–2): roughly one week of focused work.

## Notes for AI Assistants Picking This Up

When you (Claude Code or other) resume this work:

1. **Read this whole document first.** The Concepts section is required
   context for the Work Plan to make sense.
2. **Read `kubeswift_context.md`** for current project state, especially
   anything that has changed since this document was written
   (April 25, 2026).
3. **Phase 0 is mandatory before Phase 1.** Do not write documentation or
   reference manifests that have not been validated on a real cluster. The
   project's design principle of "verified fixes only" applies here too.
4. **The `Ready` condition (Phase 3) is additive, never replacing.** Existing
   conditions are part of the public API; removing them is a breaking change.
5. **Helm chart CRDs must be synced** after any CRD change:
   `make generate && cp config/crd/bases/*.yaml charts/kubeswift/crds/`.
6. **Test against the v0.2.0-rc.x chart** — this is the version that will be
   stable when GitOps work begins. Do not test against a stale chart.
7. **Do not couple KubeSwift to Flux.** KubeSwift must remain usable without
   Flux. All Flux integration is in user-facing docs and examples, not in
   KubeSwift's controllers or CRDs (with the exception of the standard
   `Ready` condition, which is generic Kubernetes convention, not Flux-specific).
