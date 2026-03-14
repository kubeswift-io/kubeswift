## Context

KubeSwift (github.com/projectbeskar/kubeswift) has existing docs in `docs/`: deploy.md, releases.md, repo-layout.md, worker-node-preflight.md, first-boot.md, operator-checklist-ubuntu-x86_64.md, and several implementation-focused docs (swiftletd-mvp, swiftguest-reconcile, seed-rendering, smoke-verification). The repository lacks a strong top-level README, a coherent architecture overview, API/CRD reference docs, and a docs index. Operators and contributors need a clear entry point and navigation. Documentation must align with the current implementation: Cloud-Hypervisor-native architecture, SwiftGuest/SwiftImage/SwiftSeedProfile/SwiftGuestClass APIs, OCI release distribution, webhook support, preflight tooling, and smoke-test flow.

## Goals / Non-Goals

**Goals:**

- Define a documentation information architecture (IA) with clear audience mapping
- Add a comprehensive README that links into deeper docs
- Add architecture documentation explaining Cloud-Hypervisor-native design and KubeVirt comparison
- Add API/CRD reference docs for SwiftGuest, SwiftGuestClass, SwiftImage, SwiftSeedProfile
- Consolidate and improve install/deployment docs for local and remote clusters
- Document release and OCI distribution clearly
- Document operator workflows (worker-node prep, smoke testing)
- Document developer workflows (repository layout, build, local dev)
- Add a docs index for navigation
- Keep docs maintainable and easy to extend

**Non-Goals:**

- New runtime features or CRD semantic changes
- Marketing site, external website, or blog generation
- Inventing features that do not exist; clearly distinguish implemented vs. planned

## Decisions

### 1. Documentation information architecture

**Decision:** Organize docs by audience and purpose:

| Audience | Purpose | Primary docs |
|----------|---------|--------------|
| Evaluators | Understand what KubeSwift is, compare to KubeVirt | README, docs/architecture.md |
| Operators | Install, deploy, operate, troubleshoot | docs/deploy.md, docs/releases.md, docs/worker-node-preflight.md, docs/operator-checklist-ubuntu-x86_64.md |
| API users | Use SwiftGuest, SwiftImage, SwiftSeedProfile, SwiftGuestClass | docs/api/ (CRD reference) |
| Developers | Build, contribute, local workflows | docs/repo-layout.md, docs/developer.md, docs/releases.md |

**Rationale:** Each audience has a clear entry point. README serves evaluators and operators; deeper docs serve specialists.

### 2. README structure and linkage

**Decision:** README.md SHALL include:

- One-line description and link to docs
- "What is KubeSwift" (brief) with link to docs/architecture.md
- "KubeSwift vs KubeVirt" (brief comparison)
- "Quick start" (OCI install command) with link to docs/deploy.md
- "Documentation" section linking to docs/index.md
- "Contributing" (link to developer docs)

README SHALL NOT duplicate content from docs; it SHALL link into docs for detail.

**Rationale:** README is the first impression; it must be scannable and route users to the right place.

### 3. Docs index

**Decision:** Add `docs/index.md` as the central documentation navigation page. It SHALL list all docs grouped by purpose: Project, Architecture, Installation, API Reference, Release, Smoke Test, Operator, Developer. Each entry: title, one-line description, path. README links to docs/index.md.

**Rationale:** Single place to discover all documentation; easy to extend when new docs are added.

### 4. API reference location and maintenance

**Decision:** API/CRD reference docs live at `docs/api/`. One file per resource: `docs/api/swiftguest.md`, `docs/api/swiftguestclass.md`, `docs/api/swiftimage.md`, `docs/api/swiftseedprofile.md`. Content SHALL derive from CRD OpenAPI schemas in `config/crd/bases/` and Go types in `api/`. Docs are hand-maintained but structured to mirror CRD fields; when CRDs change, docs must be updated. No auto-generation in this change; document the maintenance expectation.

**Rationale:** Operators need field-level reference. Hand-maintained keeps docs readable; CI can later add a check for drift.

### 5. Architecture documentation

**Decision:** Add `docs/architecture.md` covering:

- Cloud-Hypervisor-native design (no libvirt, direct CH API)
- Control plane (controller-manager, controllers, webhooks)
- Node runtime (swiftletd, pod envelope, runtime intent)
- Data flow: SwiftGuest → controller → pod + intent → swiftletd → Cloud Hypervisor
- KubeSwift vs KubeVirt (architecture, naming, scope)

**Rationale:** Evaluators and contributors need to understand the design before diving in.

### 6. Consolidation of existing docs

**Decision:** Keep existing docs (deploy.md, releases.md, repo-layout.md, worker-node-preflight.md, first-boot.md, operator-checklist-ubuntu-x86_64.md, smoke-verification.md, etc.). Improve and cross-link; do not remove. Add docs/developer.md to consolidate developer workflows (build, local deploy, version stamping, release targets). Ensure deploy.md and releases.md are the canonical install/release references.

**Rationale:** Existing docs have value; consolidation reduces fragmentation without rewriting everything.

### 7. Implemented vs. planned

**Decision:** Each doc SHALL clearly distinguish implemented behavior from planned future work. Use explicit callouts:

- **Currently supported:** Linux cloud images, one root disk, one network, NoCloud seed, start/stop/restart, SwiftGuest/SwiftImage/SwiftSeedProfile/SwiftGuestClass, OCI Helm install, admission webhooks (optional), worker-node preflight, smoke test
- **Not yet implemented:** SwiftGuestMigration, SwiftGuestSnapshot, SwiftGuestPool, ConfigDrive/Ignition/Unattend seeds, multi-disk, multi-NIC, Windows guests, live migration, snapshots

Add a "Not yet implemented" subsection in docs/architecture.md and docs/project.md (if created). In API docs, only document implemented fields and source types.

**Rationale:** Prevents user confusion and sets correct expectations.

## Risks / Trade-offs

| Risk | Mitigation |
|------|------------|
| Docs drift from implementation | Document maintenance expectation; consider CI check for API doc drift in future |
| README too long | Keep README scannable; link to docs for detail |
| Duplication across docs | README links, don't duplicate; docs index reduces orphan content |
| API docs become stale | Structure docs to mirror CRD layout; make updates part of CRD change workflow |

### 8. Exact file list

**Files to ADD:**

| Path | Purpose |
|------|---------|
| `README.md` | Top-level project entry; what is KubeSwift, vs KubeVirt, quick start, link to docs |
| `docs/index.md` | Central navigation; all docs grouped by purpose |
| `docs/project.md` | Project overview: goals, scope, implemented vs. not yet implemented |
| `docs/architecture.md` | Cloud-Hypervisor-native design, control plane, node runtime, data flow, vs KubeVirt |
| `docs/api/swiftguest.md` | SwiftGuest CRD reference |
| `docs/api/swiftguestclass.md` | SwiftGuestClass CRD reference |
| `docs/api/swiftimage.md` | SwiftImage CRD reference |
| `docs/api/swiftseedprofile.md` | SwiftSeedProfile CRD reference |
| `docs/developer.md` | Build, deploy, version stamping, release targets, contributing |

**Files to UPDATE:**

| Path | Changes |
|------|---------|
| `docs/deploy.md` | Cross-links to index, worker-node-preflight; ensure OCI/local/webhook complete |
| `docs/releases.md` | Cross-links to index, deploy |
| `docs/repo-layout.md` | Ensure current (charts/, hack/); cross-link to index |
| `docs/worker-node-preflight.md` | Cross-link to index, deploy |
| `docs/operator-checklist-ubuntu-x86_64.md` | Cross-link to index, smoke-verification |
| `docs/first-boot.md` | Cross-link to index, smoke-verification |
| `docs/smoke-verification.md` | Cross-link to index, first-boot |

**Files to KEEP (listed in index, no content changes unless needed):**

| Path | Purpose |
|------|---------|
| `docs/swiftletd-mvp.md` | Implementation detail; link from architecture/developer |
| `docs/swiftguest-reconcile.md` | Implementation detail |
| `docs/seed-rendering.md` | Implementation detail |

**Scope:** Documentation only. No code changes.

## Migration Plan

1. Add README.md
2. Add docs/index.md
3. Add docs/architecture.md
4. Add docs/api/*.md (4 files)
5. Add docs/developer.md
6. Update deploy.md, releases.md, repo-layout.md, worker-node-preflight.md, operator-checklist-ubuntu-x86_64.md, first-boot.md, smoke-verification.md with cross-links
7. Add "Currently supported" / "Not yet implemented" callouts where relevant
8. **Rollback:** Revert doc commits; no runtime impact
