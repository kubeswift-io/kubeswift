# KubeSwift Documentation

KubeSwift runs Linux and Windows VMs on Kubernetes. [Cloud Hypervisor](https://www.cloud-hypervisor.org/) is the primary hypervisor — disk boot, kernel boot, Windows guests, PCIe GPU passthrough, snapshots, and migration all run on it. QEMU is a secondary runtime used only for HGX SXM multi-GPU topologies, where CUDA requires a full PCIe hierarchy. Define guests with CRDs; the control plane reconciles them into pods; swiftletd launches the hypervisor. Two boot sources are supported: disk boot (cloud images with firmware) and kernel boot (direct bzImage + initramfs); Windows guests use `osType: windows`, a disk-boot variant.

## Documentation Index

### Architecture

- [Architecture overview](architecture.md) — Cloud-Hypervisor-first design, components, boot paths
- [Control plane](architecture/control-plane.md) — Controllers, reconciliation, admission webhooks
- [Node runtime](architecture/node-runtime.md) — swiftletd, Cloud Hypervisor, runtime intent
- [Lifecycle](architecture/lifecycle.md) — Guest lifecycle, status mapping, conditions

### API Reference

- [API overview](api/overview.md) — API groups, CRDs, versioning
- [SwiftGuest](api/swiftguest.md) — VM instance resource
- [SwiftGuestClass](api/swiftguestclass.md) — Cluster-scoped template (CPU, memory, root disk)
- [SwiftImage](api/swiftimage.md) — Disk image source (HTTP, PVC)
- [SwiftSeedProfile](api/swiftseedprofile.md) — Cloud-init datasource (NoCloud)
- [SwiftKernel](api/swiftkernel.md) — Kernel + initramfs OCI artifact
- [SwiftGuestPool](api/swiftguestpool.md) — VM fleet management
- [Data disks](api/data-disks.md) — blank / image-backed / attached-PVC secondary disks

### Kernel Boot

- [SwiftKernel reference](swiftkernel.md) — Full reference: node setup, building profiles, OCI packaging, usage
- [Kernel boot quickstart](kernel-boot-quickstart.md) — Boot a kernel VM in five steps

### vhost-user Devices

- [virtiofs & vhost-user devices](virtiofs.md) — shared filesystems (virtiofs), vhost-user-net/blk/generic (operator backends)

### Windows Guests

- [Running Windows guests](windows/overview.md) — Overview: `osType: windows`, the end-to-end lifecycle, RDP management, limitations
- [Windows image prep](windows/image-prep.md) — Operator runbook: build a virtio-ready, CH-bootable Windows image

### GPU Passthrough

- [GPU Passthrough](gpu-passthrough.md) — VFIO passthrough, compatibility tiers, GPU Discovery DaemonSet, SwiftGPUProfile reference, Fabric Manager
- [GPU allocation via DRA](gpu/dra-allocation.md) — Scheduler-allocated GPUs through ResourceClaims (`spec.gpuResourceClaim`), the reference DRA driver, CDI node prep

### Installation

- [Local cluster](install/local-cluster.md) — kind, minikube, build and deploy
- [Remote cluster](install/remote-cluster.md) — Prerequisites, OCI Helm install
- [Helm OCI](install/helm-oci.md) — Version selection, webhooks, image overrides

### Networking

- [Networking Operations Guide](networking/operations-guide.md) -- Physical networks, VLANs, bonds, isolated networks
- [Service exposure](networking/service-exposure.md) -- Expose guest ports as Kubernetes Services (`spec.network.ports`), pool-balanced Services, egress reachability
- [Virtualization Platform Comparison](networking/virtualization-comparison.md) -- VMware ESXi and Proxmox VE concept mapping
- [Multi-NIC Support](multi-nic.md) -- CRD spec, MAC generation, architecture
- [OVN-Kubernetes Integration](networking/ovn-kubernetes.md) -- Layer 2/3, localnet, UDN, CUDN
- [SR-IOV NIC Passthrough](networking/sriov.md) -- VFIO passthrough for GPUDirect RDMA, DPDK
- [Multi-node L2 (IP-preserving guests)](networking/multi-node-l2.md) -- primary-on-NAD, kube-ovn IP-preserving live migration
- [kube-ovn L2 install guide](networking/ovn-l2-install.md) -- Deploy kube-ovn + a primary NAD for zero-touch IP-preserving guests
- [Primary-UDN tenancy (Model A)](networking/udn-primary-tenancy.md) -- Guests on a namespace primary UDN: native UDN IP, cross-node, tenant-isolated

### Fleet Management

- [SwiftGuestPool Guide](swiftguestpool-guide.md) -- Scaling, rolling updates, spread, PVCs, monitoring
- [SwiftGuestPool Use Cases](swiftguestpool-use-cases.md) -- GPU inference, CI/CD runners, VDI, telco NFV, batch/HPC

### Snapshots & clones

- [Snapshots & fast VMs](snapshots/fast-vms.md) — snapshots, restore, and instant clones overview
- [CSI snapshots](snapshots/csi-snapshots.md) — disk-only backup/restore via CSI VolumeSnapshot
- [Local snapshots](snapshots/local-snapshots.md) — Tier B local memory+disk snapshots
- [S3 snapshots](snapshots/s3-snapshots.md) — Tier C cluster-portable snapshots on object storage
- [Scheduled snapshots](snapshots/scheduled-snapshots.md) — cron schedule + keep-N retention
- [clone-from-snapshot](snapshots/clone-from-snapshot.md) — fan out N VMs from one snapshot
- [Identity regeneration](snapshots/identity-regeneration.md) — regenerate a clone's identity in place (vsock agent)

The `oci` snapshot backend (`SwiftSnapshot.spec.backend.type: oci`) pushes memory+disk state to an OCI registry; it underpins cold migration. See [cold migration](snapshots/cold-migration.md).

### Migration

- [Migration overview](migration/overview.md) — offline and live migration model
- [Migratable guests](migration/migratable-guests.md) — RWX+Block storage for live migration
- [Cold migration](snapshots/cold-migration.md) — move a suspended VM's full state between nodes/clusters via an OCI registry (`swiftctl guest export`/`import`)
- [Networking requirements](migration/networking-requirements.md) — IP preservation vs `allowIPChange`
- [Troubleshooting](migration/troubleshooting.md) — migration failure modes

### Registry / golden images

- [Golden images](registry/golden-images.md) — publish VM images to an OCI registry (`swiftctl image publish`, `SwiftImage.spec.source.oci`)
- [Edge Zot registry](registry/edge-zot.md) — per-site Zot mirroring VM artifacts from a hub (`zot sync`), including air-gap feeding

### Gateway & Web UI

- [Gateway](ui/gateway.md) — read/action API for the web UI; multi-cluster fleet federation
- [Gateway auth](ui/auth.md) — user authentication and RBAC impersonation

### Operator

- [swiftctl](swiftctl.md) — Operator CLI for SwiftGuest lifecycle and console access
- [First boot (disk)](first-boot.md) — Boot a cloud image VM
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

### Contributing

- [Kernel profiles](contributing/kernel-profiles.md) — Guide for adding new kernel profiles

### GitOps

- [GitOps with FluxCD](gitops/README.md) — three-layer model, quickstart, secrets, troubleshooting; reference repo in `examples/gitops-flux/`

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
| Boot a cloud image VM | [First boot](first-boot.md) |
| Boot a kernel VM | [Kernel boot quickstart](kernel-boot-quickstart.md) |
| Run smoke test | [Smoke verification](operator/smoke-verification.md) |
| Validate worker node | [Worker-node preflight](operator/worker-node-preflight.md) |
| Connect VMs to physical networks | [Networking Operations Guide](networking/operations-guide.md) |
| Pass a GPU through to a VM | [GPU Passthrough](gpu-passthrough.md) / [via DRA](gpu/dra-allocation.md) |
| Snapshot or clone a VM | [Snapshots & fast VMs](snapshots/fast-vms.md) |
| Migrate a VM between nodes | [Migration overview](migration/overview.md) |
| Publish a golden VM image | [Golden images](registry/golden-images.md) |
| Migrate from VMware/Proxmox | [Virtualization Comparison](networking/virtualization-comparison.md) |
| Build locally | [Build](developer/build.md) |
| Understand CRDs | [API overview](api/overview.md) |
| Add a kernel profile | [Contributing kernel profiles](contributing/kernel-profiles.md) |
