# KubeSwift Project Documentation Specification

## ADDED Requirements

### Requirement: Top-level README exists and links to docs

The repository SHALL provide a top-level README.md that explains what KubeSwift is, how it differs from KubeVirt, and how to get started. The README SHALL link to docs/index.md (docs index) and to docs/deploy.md for installation. The README SHALL NOT duplicate detailed content from docs; it SHALL route users to the appropriate doc.

#### Scenario: Evaluator reads README

- **WHEN** a user opens the repository README
- **THEN** they see a one-line description of KubeSwift
- **AND** they see a brief "KubeSwift vs KubeVirt" comparison
- **AND** they see a "Quick start" or install command with a link to full install docs

#### Scenario: README links to docs index

- **WHEN** a user follows the documentation link from the README
- **THEN** they reach docs/index.md
- **AND** the index lists all documentation grouped by purpose

### Requirement: Documentation index exists

The repository SHALL provide docs/index.md as the central documentation navigation page. The index SHALL list all docs grouped by purpose: Project, Architecture, Installation, API Reference, Release, Smoke Test, Operator, Developer. Each entry SHALL include a title, one-line description, and path. The index SHALL be the primary navigation entry point for docs/.

#### Scenario: User navigates from index

- **WHEN** a user opens docs/index.md
- **THEN** they see all docs listed by category
- **AND** each entry links to the corresponding doc
- **AND** categories include Project, Architecture, Installation, API Reference, Release, Smoke Test, Operator, Developer

### Requirement: Project documentation exists

The repository SHALL provide docs/project.md with a project overview. The doc SHALL describe goals, scope, "Currently supported" (implemented features), and "Not yet implemented" (planned features). The doc SHALL NOT describe unimplemented features as available.

#### Scenario: Evaluator reads project doc

- **WHEN** a user reads docs/project.md
- **THEN** they see what KubeSwift currently supports (SwiftGuest, SwiftImage, SwiftSeedProfile, SwiftGuestClass, OCI install, webhooks, preflight, smoke test)
- **AND** they see what is not yet implemented (migration, snapshots, SwiftGuestPool, etc.)

### Requirement: Architecture documentation exists

The repository SHALL provide docs/architecture.md that describes the Cloud-Hypervisor-native design. The doc SHALL cover: control plane (controller-manager, controllers, webhooks), node runtime (swiftletd, pod envelope, runtime intent), data flow from SwiftGuest to Cloud Hypervisor, and KubeSwift vs KubeVirt comparison. The doc SHALL distinguish implemented behavior from planned future work.

#### Scenario: Evaluator reads architecture

- **WHEN** a user reads docs/architecture.md
- **THEN** they understand that KubeSwift uses Cloud Hypervisor directly (no libvirt)
- **AND** they understand the control-plane vs node-runtime split
- **AND** they see how KubeSwift differs from KubeVirt in architecture and naming

### Requirement: API and CRD reference docs exist

The repository SHALL provide API/CRD reference documentation for SwiftGuest, SwiftGuestClass, SwiftImage, and SwiftSeedProfile. The docs SHALL live at docs/api/ with one file per resource (swiftguest.md, swiftguestclass.md, swiftimage.md, swiftseedprofile.md). Each doc SHALL describe the resource purpose, spec fields, status fields, and include concrete examples. Content SHALL align with config/crd/bases/ and api/ types.

#### Scenario: Operator looks up SwiftGuest fields

- **WHEN** an operator opens docs/api/swiftguest.md
- **THEN** they see spec fields (guestClassRef, imageRef, runPolicy, etc.) and status fields
- **AND** they see at least one example manifest
- **AND** the doc reflects the current CRD schema

#### Scenario: Operator looks up SwiftImage fields

- **WHEN** an operator opens docs/api/swiftimage.md
- **THEN** they see spec fields (format, source, etc.) and status
- **AND** they see example source types (http, pvc, etc.)

### Requirement: Install and deployment docs exist

The repository SHALL provide install and deployment documentation for local and remote clusters. The doc SHALL cover OCI Helm install (remote), local build/deploy (kind/minikube), and optional webhook enablement. The doc SHALL reference docs/worker-node-preflight.md for worker-node preparation. The canonical install doc SHALL be docs/deploy.md.

#### Scenario: Operator installs from OCI

- **WHEN** an operator follows the install docs for a remote cluster
- **THEN** they see the helm install command for oci://ghcr.io/projectbeskar/charts/kubeswift
- **AND** they see version selection (dev, RC, stable)
- **AND** they see optional webhook and image override instructions

#### Scenario: Developer deploys locally

- **WHEN** a developer follows the install docs for local clusters
- **THEN** they see make build-images, make load-images, make deploy
- **AND** they see a link to worker-node preflight

### Requirement: Release and OCI distribution docs exist

The repository SHALL provide release and OCI distribution documentation. The doc SHALL explain version stamping, release types (dev, RC, stable), workflow triggers, Makefile targets (release-dev, release-rc, release-stable), and chart/image tagging. The canonical release doc SHALL be docs/releases.md.

#### Scenario: Operator selects version

- **WHEN** an operator reads the release docs
- **THEN** they understand dev (0.0.0-dev.<shortsha>), RC, and stable version formats
- **AND** they see how to choose a chart version for install

#### Scenario: Developer runs release targets

- **WHEN** a developer reads the release docs
- **THEN** they see make release-dev, release-rc, release-stable
- **AND** they see hack/version.sh and hack/chart-version.sh usage

### Requirement: Smoke-test and operator docs exist

The repository SHALL provide smoke-test and operator documentation. Smoke-test docs: docs/first-boot.md (walkthrough), docs/smoke-verification.md (prerequisites, verification, failure checks). Operator docs: docs/worker-node-preflight.md (preflight script), docs/operator-checklist-ubuntu-x86_64.md (Ubuntu host setup). These docs MAY be existing files; they SHALL be listed in docs/index.md under Smoke Test and Operator sections and cross-linked.

#### Scenario: Operator prepares worker node

- **WHEN** an operator prepares a worker node for KubeSwift
- **THEN** they find docs/worker-node-preflight.md and docs/operator-checklist-ubuntu-x86_64.md via docs/index.md
- **AND** the docs describe preflight checks and Ubuntu host requirements

#### Scenario: Operator runs smoke test

- **WHEN** an operator runs a smoke test
- **THEN** they find docs/first-boot.md and docs/smoke-verification.md via docs/index.md
- **AND** the docs describe the sample flow (SwiftGuestClass, SwiftImage, SwiftSeedProfile, SwiftGuest) and verification steps

### Requirement: Developer docs exist for repository layout and workflows

The repository SHALL provide developer documentation covering repository layout, build flow, and local workflows. The doc SHALL reference docs/repo-layout.md for layout and SHALL add or extend docs/developer.md for build (make build, make build-images), local deploy (make deploy, make load-images), version stamping, and release targets. The doc SHALL explain how to contribute and where to find implementation details.

#### Scenario: Developer builds locally

- **WHEN** a developer reads the developer docs
- **THEN** they see make build, make build-go, make build-rust, make build-images
- **AND** they see make deploy and make load-images for kind/minikube
- **AND** they see docs/repo-layout.md for directory structure

#### Scenario: Developer understands version stamping

- **WHEN** a developer reads the developer docs
- **THEN** they see how Go (ldflags) and Rust (build.rs) receive version metadata
- **AND** they see hack/version.sh and print-version usage

### Requirement: Docs distinguish implemented from planned

Each documentation file SHALL clearly distinguish implemented behavior from planned future work. Where relevant, use explicit sections or callouts (e.g., "Currently supported", "Planned") so that users do not assume unimplemented features are available.

#### Scenario: User reads implemented scope

- **WHEN** a user reads any doc that describes features
- **THEN** they can identify what is implemented vs. planned (via "Currently supported" / "Not yet implemented" callouts)
- **AND** unimplemented features (SwiftGuestMigration, SwiftGuestSnapshot, SwiftGuestPool, ConfigDrive, multi-disk, etc.) are explicitly marked as not yet implemented
