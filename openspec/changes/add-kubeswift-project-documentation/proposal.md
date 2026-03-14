## Why

KubeSwift has reached a maturity level where the repository must present a production-quality documentation baseline. The project now includes a coherent architecture (Cloud-Hypervisor-native, Kubernetes-native), implemented APIs (SwiftGuest, SwiftGuestClass, SwiftImage, SwiftSeedProfile), OCI release distribution, admission webhooks, worker-node preflight tooling, and smoke-test flows. Without a strong documentation set, operators cannot evaluate, install, or operate KubeSwift effectively; contributors cannot onboard; and the project cannot be taken seriously as a production-ready virtualization platform. Documentation is now a blocking project requirement for adoption and contribution.

## What Changes

- Add a comprehensive top-level README.md that explains what KubeSwift is, how it differs from KubeVirt, and how to get started
- Add or improve architecture documentation describing the Cloud-Hypervisor-native design
- Add API and CRD documentation for SwiftGuest, SwiftGuestClass, SwiftImage, and SwiftSeedProfile
- Add install and deployment documentation for local and remote clusters (consolidate and extend existing deploy.md)
- Add release and OCI distribution documentation (extend releases.md)
- Add operator documentation for worker-node preparation and smoke testing
- Add developer documentation for repository layout, build flow, and local workflows
- Add docs/index.md as the central documentation navigation page

**Out of scope:** New runtime features, CRD semantic changes, marketing site or external website generation, blog posts or announcement copy.

## Capabilities

### New Capabilities

- `kubeswift-project-documentation`: Defines the documentation information architecture, document structure, audience mapping, and requirements for project, operator, and developer documentation. Ensures README, architecture, API reference, install, release, operator, and developer docs exist and are maintainable.

### Modified Capabilities

- None. This change adds documentation only; no spec-level behavior changes for existing capabilities.

## Impact

- **Paths:** `README.md`, `docs/` (new and modified files)
- **Files to add:** README.md, docs/index.md, docs/project.md, docs/architecture.md, docs/api/swiftguest.md, docs/api/swiftguestclass.md, docs/api/swiftimage.md, docs/api/swiftseedprofile.md, docs/developer.md
- **Files to update:** docs/deploy.md, docs/releases.md, docs/repo-layout.md, docs/worker-node-preflight.md, docs/operator-checklist-ubuntu-x86_64.md, docs/first-boot.md, docs/smoke-verification.md
- **Scope:** Documentation only; no code changes
- **Maintenance:** Docs must be kept aligned with implementation; API reference docs derive from CRDs
- **Audiences:** Evaluators (project, architecture), operators (install, smoke-test, operator), API users (CRD reference), developers (build, contribute)
- **Risks:** Drift between docs and code
- **Rollback:** Revert doc commits; no runtime impact
