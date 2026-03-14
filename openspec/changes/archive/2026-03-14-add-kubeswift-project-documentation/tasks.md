## 1. Top-Level README

- [x] 1.1 Create `README.md` with one-line description of KubeSwift
- [x] 1.2 Add "What is KubeSwift" section (brief) with link to docs/architecture.md
- [x] 1.3 Add "KubeSwift vs KubeVirt" section (brief comparison)
- [x] 1.4 Add "Quick start" section with OCI install command and link to docs/install/
- [x] 1.5 Add "Documentation" section linking to docs/index.md
- [x] 1.6 Add "Contributing" section linking to docs/developer/

## 2. Docs Index

- [x] 2.1 Create `docs/index.md` as the central navigation page
- [x] 2.2 Add sections: Architecture, API Reference, Installation, Operator, Developer, Release
- [x] 2.3 List all docs with title, one-line description, and path for each section

## 3. Project Documentation

- [x] 3.1 Project overview in README and docs/architecture.md (goals, scope)
- [x] 3.2 "Currently supported" in README (Linux cloud images, CRDs, OCI install, webhooks, preflight, smoke test)
- [x] 3.3 "Not yet implemented" in README (SwiftGuestMigration, SwiftGuestSnapshot, SwiftGuestPool, ConfigDrive/Ignition, multi-disk, multi-NIC, Windows, migration, snapshots)

## 4. Architecture Documentation

- [x] 4.1 Create `docs/architecture.md` with Cloud-Hypervisor-native design overview
- [x] 4.2 Document control plane in `docs/architecture/control-plane.md`
- [x] 4.3 Document node runtime in `docs/architecture/node-runtime.md`
- [x] 4.4 Document data flow in docs/architecture.md
- [x] 4.5 Add KubeSwift vs KubeVirt comparison in README
- [x] 4.6 Add "Currently supported" and "Not yet implemented" in README

## 5. API Reference Documentation

- [x] 5.1 Create `docs/api/` directory with overview.md
- [x] 5.2 Create `docs/api/swiftguest.md` with spec/status fields, examples
- [x] 5.3 Create `docs/api/swiftguestclass.md` with spec/status fields, examples
- [x] 5.4 Create `docs/api/swiftimage.md` with spec/status fields, source types (http, pvc), examples
- [x] 5.5 Create `docs/api/swiftseedprofile.md` with spec/status fields, datasource (NoCloud), examples

## 6. Developer Documentation

- [x] 6.1 Create `docs/developer/getting-started.md` and `build.md` with build flow
- [x] 6.2 Add local deploy section in docs/install/local-cluster.md
- [x] 6.3 Add version stamping section in docs/developer/build.md and docs/releases.md
- [x] 6.4 Add release targets section in docs/releases.md
- [x] 6.5 Create `docs/developer/repo-layout.md` for directory structure
- [x] 6.6 Add "Contributing" in README; link to docs/developer/ and implementation docs (swiftletd-mvp, swiftguest-reconcile, seed-rendering)

## 7. Install Doc Updates

- [x] 7.1 Create docs/install/ (local-cluster, remote-cluster, helm-oci); update docs/deploy.md with cross-links
- [x] 7.2 OCI install, local deploy, webhook sections in docs/install/helm-oci.md

## 8. Release Doc Updates

- [x] 8.1 Create `docs/releases.md` with cross-links to docs/index.md and docs/install/
- [x] 8.2 Version stamping, release types, Makefile targets documented

## 9. Smoke-Test and Operator Doc Updates

- [x] 9.1 Create `docs/operator/smoke-verification.md` with quick walkthrough and cross-links
- [x] 9.2 First-boot content merged into smoke-verification.md
- [x] 9.3 Create `docs/operator/worker-node-preflight.md` (fixed script ref: kubeswift-preflight.sh)
- [x] 9.4 Create `docs/operator/operator-checklist-ubuntu-x86_64.md` with cross-links
- [x] 9.5 Create `docs/operator/troubleshooting.md`

## 10. Repo Layout and Index Finalization

- [x] 10.1 Create `docs/developer/repo-layout.md` with charts/, hack/; cross-link to docs/index.md
- [x] 10.2 docs/index.md includes architecture, api/*, install/*, operator/*, developer/*, releases
- [x] 10.3 Cross-links verified; "Not yet implemented" in README and API docs
