# KubeSwift

KubeSwift runs virtual machines as first-class Kubernetes workloads. You define a VM with a custom resource; controllers reconcile it into a pod; inside that pod, `swiftletd` launches a hypervisor. The default hypervisor is [Cloud Hypervisor](https://www.cloud-hypervisor.org/); QEMU is used automatically for GPU workloads that need a specific PCIe topology.

It is **not** a container sandbox (not Kata Containers) — each guest is a real VM, one per pod.

## Capabilities

- **Boot paths** — disk boot from cloud images (Ubuntu, Rocky, Debian, Fedora) and direct kernel boot from OCI artifacts (sub-second microVMs). Windows guests via `osType: windows`.
- **GPU passthrough** — whole-GPU VFIO passthrough with two allocation backends: the native SwiftGPU model (discovery DaemonSet + profiles) or Kubernetes [DRA](docs/gpu/dra-allocation.md) ResourceClaims. PCIe GPUs on Cloud Hypervisor; HGX SXM on QEMU.
- **Networking** — tap + bridge + DHCP with the guest IP surfaced in status; multi-NIC via Multus; SR-IOV NIC passthrough; OVN-Kubernetes and multi-node L2.
- **Storage** — per-guest root-disk cloning sized from a class; optional data disks; RWX+Block for live-migration-capable volumes.
- **Snapshots & clones** — disk-only (CSI) and memory+disk (local/S3) snapshots, scheduled snapshots, and `cloneFromSnapshot` for fast VM fan-out.
- **Migration** — offline migration on any storage, and live migration (sub-second downtime) with optional mTLS transport and `kubectl drain` integration.
- **Fleets** — `SwiftGuestPool` gives ReplicaSet-style scaling with rolling updates, topology spread, and a PVC per replica.
- **Operations** — `swiftctl` for console/SSH/lifecycle/describe; Prometheus metrics and Grafana dashboards across every feature; cloud-init via NoCloud; security-hardened containers (drop-ALL, no privileged).

## Custom Resources

| CRD | Short | API group | Scope | Purpose |
|-----|-------|-----------|-------|---------|
| SwiftGuest | `sg` | `swift.kubeswift.io` | Namespaced | A VM instance |
| SwiftGuestClass | `sgc` | `swift.kubeswift.io` | Cluster | CPU/memory/disk template |
| SwiftGuestPool | `sgpool` | `swift.kubeswift.io` | Namespaced | Fleet of identical VMs |
| SwiftImage | `si` | `image.kubeswift.io` | Namespaced | Disk image source |
| SwiftSeedProfile | `ssp` | `seed.kubeswift.io` | Namespaced | cloud-init (NoCloud) config |
| SwiftKernel | `sk` | `kernel.kubeswift.io` | Namespaced | Kernel + initramfs OCI artifact |
| SwiftGPUProfile | `sgp` | `gpu.kubeswift.io` | Namespaced | GPU passthrough request (native backend) |
| SwiftGPUNode | `sgn` | `gpu.kubeswift.io` | Cluster | Per-node GPU inventory |
| SwiftSnapshot | — | `snapshot.kubeswift.io` | Namespaced | VM snapshot (disk or memory+disk) |
| SwiftRestore | — | `snapshot.kubeswift.io` | Namespaced | Restore from a snapshot |
| SwiftSnapshotSchedule | — | `snapshot.kubeswift.io` | Namespaced | Cron-scheduled snapshots + keep-N |
| SwiftMigration | — | `migration.kubeswift.io` | Namespaced | Move a guest between nodes |

12 CRDs, all `v1alpha1`.

## Documentation

Start at the **[documentation index](docs/index.md)**. Common entry points:

- [Quickstart](docs/quickstart.md) — boot your first VM
- [Architecture](docs/architecture.md) — components, boot paths, status model
- [CRD reference](docs/crds.md) · [API overview](docs/api/overview.md)
- [GPU passthrough](docs/gpu-passthrough.md) · [GPU via DRA](docs/gpu/dra-allocation.md)
- [Networking operations](docs/networking/operations-guide.md)
- [Snapshots](docs/snapshots/csi-snapshots.md) · [Live migration](docs/migration/overview.md)
- [swiftctl CLI](docs/swiftctl.md) · [Observability](docs/observability/README.md)
- [Install (Helm/OCI)](docs/install/helm-oci.md)

## Build

```bash
make build          # Go binaries
make build-images   # container images
make deploy         # apply CRDs + deploy the controller
go test ./...       # Go tests
cargo test          # Rust tests (from rust/)
```

## Status

Pre-1.0 and Linux x86_64 only; the `v1alpha1` API may change between releases. The disk/kernel/QEMU boot paths, networking, snapshots, offline and live migration, SwiftGuestPool, and Tier-1 PCIe GPU passthrough are validated on a live cluster. Hardware-gated items (HGX/Tier-2 GPU, SR-IOV NICs, SEV-SNP confidential VMs) are implemented or designed but await hardware.

## License

Licensed under the [GNU Affero General Public License v3.0](LICENSE) (AGPL-3.0).
