# KubeSwift Documentation

KubeSwift is a Kubernetes-native VM runtime built on Cloud Hypervisor (default) and QEMU (GPU workloads). VMs are first-class Kubernetes workloads defined by CRDs and reconciled by controllers. Each guest runs as a pod; swiftletd inside that pod launches the hypervisor.

## Quick navigation

| I want to... | Go here |
|-------------|---------|
| Get started fast | [quickstart.md](quickstart.md) |
| Understand how it works | [architecture.md](architecture.md) |
| See the architecture visually | [architecture/diagrams.md](architecture/diagrams.md) |
| Look up a CRD field | [crds.md](crds.md) |
| Connect VMs to physical networks | [networking/operations-guide.md](networking/operations-guide.md) |
| Migrate from VMware/Proxmox | [networking/virtualization-comparison.md](networking/virtualization-comparison.md) |
| Set up GPU passthrough | [gpu-passthrough.md](gpu-passthrough.md) |
| Create fast VMs with snapshots & clones | [snapshots/fast-vms.md](snapshots/fast-vms.md) |
| Live-migrate VMs between nodes | [migration/migratable-guests.md](migration/migratable-guests.md) |
| Manage VM fleets | [swiftguestpool-guide.md](swiftguestpool-guide.md) |
| Use the swiftctl CLI | [swiftctl.md](swiftctl.md) |
| Monitor with Prometheus & Grafana | [observability/README.md](observability/README.md) |
| Build and contribute | [development.md](development.md) |

---

## Documents

### [quickstart.md](quickstart.md)
Getting started guide. Install KubeSwift, boot your first disk-boot VM (Ubuntu Noble), boot a kernel-boot microVM (faas-minimal), connect via console and SSH, and run lifecycle commands.

### [architecture.md](architecture.md)
Comprehensive architecture reference. System diagram, control plane components (SwiftImage, SwiftKernel, SwiftGPU, SwiftGuest controllers), runtime plane (swiftletd, hypervisor dispatch, serial socket), all three boot paths, networking model (tap + bridge + dnsmasq), status reporting via pod annotations, and GPU architecture.

### [crds.md](crds.md)
Full CRD reference for all 7 resources: SwiftGuest, SwiftGuestClass, SwiftImage, SwiftSeedProfile, SwiftKernel, SwiftGPUProfile, SwiftGPUNode. Covers every spec and status field, types, defaults, validation rules, mutual exclusivity rules, and example manifests.

### [gpu-passthrough.md](gpu-passthrough.md)
GPU operator guide. Prerequisites (IOMMU, vfio-pci, Fabric Manager), GPU compatibility tier table, step-by-step workflow (label node → discovery → profile → guest), SwiftGPUProfile examples for each tier, SwiftGPUNode inspection, Fabric Manager setup, and troubleshooting.

### [swiftguestpool-guide.md](swiftguestpool-guide.md)
SwiftGuestPool operational guide. Creating pools, scaling, rolling updates, high availability with topology spread, persistent data with volumeClaimTemplates, GPU inference fleets, CI/CD runner pools, monitoring, and troubleshooting.

### [swiftctl.md](swiftctl.md)
CLI reference for all commands: `console`, `ssh`, `start`, `stop`, `restart`, `describe`, `logs`, `debug`. Covers flags, behavior, requirements, examples, and transport details for console and SSH.

### [development.md](development.md)
Contributor guide. Repository structure, build commands (Go + Rust + images), deploy workflow, CRD workflow (make generate + copy + redeploy), test commands, design principles, adding new controllers/types/crates, debugging procedures, and known version constraints.

---

## Additional docs (existing)

The `docs/` directory also contains these focused reference documents:

### Architecture

- [architecture/control-plane.md](architecture/control-plane.md) — Controller reconciliation details
- [architecture/node-runtime.md](architecture/node-runtime.md) — swiftletd flow, mount paths, environment
- [architecture/lifecycle.md](architecture/lifecycle.md) — Guest lifecycle, status mapping, conditions

### API reference

- [api/overview.md](api/overview.md) — API groups, versioning
- [api/swiftguest.md](api/swiftguest.md) — SwiftGuest deep-dive
- [api/data-disks.md](api/data-disks.md) — Data disks (blank / image-backed / attached-PVC)
- [api/swiftimage.md](api/swiftimage.md) — SwiftImage deep-dive
- [api/swiftkernel.md](api/swiftkernel.md) — SwiftKernel deep-dive

### Fleet Management

- [swiftguestpool-guide.md](swiftguestpool-guide.md) — Operational guide: scaling, rolling updates, spread, PVCs
- [swiftguestpool-use-cases.md](swiftguestpool-use-cases.md) — GPU inference, CI/CD runners, VDI, telco NFV, batch/HPC
- [api/swiftguestpool.md](api/swiftguestpool.md) — SwiftGuestPool API reference

### Snapshots, clones & identity

- [snapshots/fast-vms.md](snapshots/fast-vms.md) — Snapshots, restore, and instant clones overview
- [snapshots/clone-from-snapshot.md](snapshots/clone-from-snapshot.md) — `cloneFromSnapshot`: fan out N VMs from one snapshot
- [snapshots/cold-migration.md](snapshots/cold-migration.md) — Cold / suspended-state migration: move a VM's full state (memory + disk) between nodes/clusters via an OCI registry (`swiftctl guest export`/`import`)
- [snapshots/identity-regeneration.md](snapshots/identity-regeneration.md) — Regenerate a clone's identity in place (the in-guest vsock agent)
- [snapshots/local-snapshots.md](snapshots/local-snapshots.md) — Tier B (local) memory snapshots
- [snapshots/s3-snapshots.md](snapshots/s3-snapshots.md) — Tier C (S3) cluster-portable snapshots
- [snapshots/scheduled-snapshots.md](snapshots/scheduled-snapshots.md) — Cron-scheduled snapshots + keep-N retention
- [registry/edge-zot.md](registry/edge-zot.md) — Edge registry profile: per-site Zot mirroring VM artifacts from a hub (`zot sync`), incl. air-gap feeding

### Kernel boot

- [swiftkernel.md](swiftkernel.md) — SwiftKernel full reference: node setup, profiles, OCI packaging
- [kernel-boot-quickstart.md](kernel-boot-quickstart.md) — Kernel boot in five steps

### Operator

- [first-boot.md](first-boot.md) — Boot a cloud image VM
- [operator/smoke-verification.md](operator/smoke-verification.md) — Smoke test details
- [operator/worker-node-preflight.md](operator/worker-node-preflight.md) — Host readiness validation
- [operator/observability.md](operator/observability.md) — Prometheus metrics
- [operator/troubleshooting.md](operator/troubleshooting.md) — Common issues

### Install

- [install/local-cluster.md](install/local-cluster.md) — kind/minikube setup
- [install/remote-cluster.md](install/remote-cluster.md) — Remote cluster prerequisites
- [install/helm-oci.md](install/helm-oci.md) — Helm OCI chart install

### Developer

- [developer/getting-started.md](developer/getting-started.md) — Clone and first build
- [developer/build.md](developer/build.md) — Build targets
- [developer/repo-layout.md](developer/repo-layout.md) — Directory structure
- [developer/testing.md](developer/testing.md) — Test commands

### Release

- [releases.md](releases.md) — Version stamping, release types, CI workflows

---

## CRD short names

```bash
kubectl get sg       # SwiftGuest
kubectl get sgc      # SwiftGuestClass
kubectl get si       # SwiftImage
kubectl get ssp      # SwiftSeedProfile
kubectl get sk       # SwiftKernel
kubectl get sgpool   # SwiftGuestPool
kubectl get sgp      # SwiftGPUProfile
kubectl get sgn      # SwiftGPUNode
```
