# KubeSwift Documentation

KubeSwift runs Linux VMs on Kubernetes using [Cloud Hypervisor](https://www.cloud-hypervisor.org/) as the sole hypervisor. Define guests with CRDs; the control plane reconciles them into pods; swiftletd launches Cloud Hypervisor.

## Documentation Index

### Architecture

- [Architecture overview](architecture.md) — Cloud-Hypervisor-native design, components, data flow
- [Control plane](architecture/control-plane.md) — Controllers, reconciliation, admission webhooks
- [Node runtime](architecture/node-runtime.md) — swiftletd, Cloud Hypervisor, runtime intent
- [Lifecycle](architecture/lifecycle.md) — Guest lifecycle, status mapping, conditions

### API Reference

- [API overview](api/overview.md) — API groups, CRDs, versioning
- [SwiftGuest](api/swiftguest.md) — VM instance resource
- [SwiftGuestClass](api/swiftguestclass.md) — Cluster-scoped template (CPU, memory, root disk)
- [SwiftImage](api/swiftimage.md) — Disk image source (HTTP, PVC)
- [SwiftSeedProfile](api/swiftseedprofile.md) — Cloud-init datasource (NoCloud)

### Installation

- [Local cluster](install/local-cluster.md) — kind, minikube, build and deploy
- [Remote cluster](install/remote-cluster.md) — Prerequisites, OCI Helm install
- [Helm OCI](install/helm-oci.md) — Version selection, webhooks, image overrides

### Operator

- [swiftctl](swiftctl.md) — Operator CLI for SwiftGuest lifecycle and console access
- [Observability](operator/observability.md) — Metrics, Prometheus integration, log collection
- [Worker-node preflight](operator/worker-node-preflight.md) — Host readiness validation script
- [Operator checklist (Ubuntu x86_64)](operator/operator-checklist-ubuntu-x86_64.md) — Host prerequisites for smoke test
- [Smoke verification](operator/smoke-verification.md) — Prerequisites, stages, failure checks, quick walkthrough
- [Troubleshooting](operator/troubleshooting.md) — Common issues and remediation

### Developer

- [Getting started](developer/getting-started.md) — Prerequisites, clone, first build
- [Build](developer/build.md) — Images, binaries, Makefile targets
- [Repo layout](developer/repo-layout.md) — Directory structure, config, Rust crates
- [Testing](developer/testing.md) — Smoke test, unit tests

### Release

- [Releases](releases.md) — Version stamping, release types, Makefile targets, CI workflows

### Implementation design (reference)

- [swiftletd MVP](swiftletd-mvp.md) — Node daemon flow, mount paths, environment
- [SwiftGuest reconcile](swiftguest-reconcile.md) — Controller reconciliation flow
- [Seed rendering](seed-rendering.md) — NoCloud control-plane vs node flow

---

## Quick links

| Task | Document |
|------|----------|
| Install from OCI | [Helm OCI](install/helm-oci.md) |
| Run smoke test | [Smoke verification](operator/smoke-verification.md) |
| Validate worker node | [Worker-node preflight](operator/worker-node-preflight.md) |
| Build locally | [Build](developer/build.md) |
| Understand CRDs | [API overview](api/overview.md) |
