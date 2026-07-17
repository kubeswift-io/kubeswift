# KubeSwift

KubeSwift runs lightweight virtual machines as native Kubernetes workloads using [Cloud Hypervisor](https://www.cloudhypervisor.org/). Use it for fast Linux and Windows VMs, GPU passthrough, VM fleets, snapshots, and live migration — without adopting a traditional virtualization stack.

## Why KubeSwift

1. **Cloud Hypervisor first** — the hypervisor for nearly every workload: disk-boot VMs, direct kernel-boot microVMs, and ephemeral OCI-image sandboxes ([SwiftSandbox](docs/sandbox/overview.md)). QEMU is a secondary runtime, used only for HGX SXM (multi-GPU NVSwitch) topologies that need a PCIe hierarchy Cloud Hypervisor doesn't yet provide.
2. **Kubernetes-native operations** — VMs are CRDs, reconciled by controllers like any other workload: `kubectl`, Services, fleets (`SwiftGuestPool`), GitOps, and Prometheus/Grafana observability all apply directly.
3. **Modern infrastructure workloads** — GPU passthrough (native SwiftGPU or Kubernetes DRA), live migration with sub-second downtime, and OCI-registry-distributed VM artifacts (golden images, snapshots, cold migration).

## Get started

```bash
helm install kubeswift oci://ghcr.io/kubeswift-io/charts/kubeswift \
  --version 0.12.0 \
  -n kubeswift-system \
  --create-namespace
```

Boot your first VM → [Quickstart](docs/quickstart.md).

**Host requirement:** x86_64 Linux nodes with `/dev/kvm` (KVM).

## Capabilities

- **Boot paths** — disk boot from cloud images (Linux and Windows) and direct kernel boot from OCI artifacts (sub-second microVMs).
- **GPU passthrough** — whole-GPU VFIO passthrough with two allocation backends: the native SwiftGPU model (discovery DaemonSet + profiles) or Kubernetes [DRA](docs/gpu/dra-allocation.md) ResourceClaims. PCIe GPUs on Cloud Hypervisor; HGX SXM on QEMU.
- **Networking** — tap + bridge + DHCP with the guest IP surfaced in status; multi-NIC via Multus; SR-IOV NIC passthrough; OVN backends supported: OVN-Kubernetes and multi-node L2 with IP-preserving cross-node live migration (kube-ovn primary-on-NAD — see [`docs/networking/ovn-l2-install.md`](docs/networking/ovn-l2-install.md)).
- **Services** — expose guest ports as Kubernetes Services via `spec.network.ports` (ClusterIP/NodePort/LoadBalancer), a load-balanced Service across pool replicas via `SwiftGuestPool.spec.service`, and a VM→cluster egress reachability probe surfaced as `EgressReady`.
- **Storage** — per-guest root-disk cloning sized from a class; optional data disks (blank/sized, image-backed, or attached PVC); RWX+Block for live-migration-capable volumes.
- **Snapshots & clones** — disk-only (CSI) and memory+disk (local/S3) snapshots, scheduled snapshots, and `cloneFromSnapshot` for fast VM fan-out.
- **Sandboxes** — ephemeral OCI-rootfs microVMs ([SwiftSandbox](docs/sandbox/overview.md)) for CI runners, agent/code execution, and untrusted code; restricted-by-default egress, cosign verify-before-boot, and a block or virtio-fs rootfs. Warm pools (`SwiftSandboxPool`) keep pre-booted slots ready for sub-second checkout. [GPU sandboxes](docs/sandbox/gpu-sandboxes.md) pass a GPU through via the native SwiftGPU or DRA backend, with warm GPU pools and model preload (`spec.model`) for sub-second inference starts; `spec.scratchDisk` attaches a block disk for build caches or dataset staging.
- **OCI registry artifacts** — golden VM images (`SwiftImage.spec.source.oci` + `swiftctl image publish`, cosign-signed with verify-on-pull) and VM snapshots / full-state cold migration (`SwiftSnapshot` `backend.type: oci`) stored in any OCI registry, for cross-cluster and edge distribution.
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
| SwiftSnapshot | ssnap | `snapshot.kubeswift.io` | Namespaced | VM snapshot (disk or memory+disk) |
| SwiftRestore | srst | `snapshot.kubeswift.io` | Namespaced | Restore from a snapshot |
| SwiftSnapshotSchedule | — | `snapshot.kubeswift.io` | Namespaced | Cron-scheduled snapshots + keep-N |
| SwiftMigration | smig | `migration.kubeswift.io` | Namespaced | Move a guest between nodes |
| SwiftSandbox | `sbox` | `sandbox.kubeswift.io` | Namespaced | Ephemeral OCI-rootfs microVM |
| SwiftSandboxPool | `sboxpool` | `sandbox.kubeswift.io` | Namespaced | Warm pool of pre-booted sandboxes for sub-second checkout |
| Cluster | `ksc` | `fleet.kubeswift.io` | Namespaced | Member cluster federated by the gateway hub |

15 CRDs, all `v1alpha1`.

## Documentation

Start at the **[documentation index](docs/index.md)**. Common entry points:

- [Quickstart](docs/quickstart.md) — boot your first VM
- [Architecture](docs/architecture.md) — components, boot paths, status model
- [CRD reference](docs/crds.md) · [API overview](docs/api/overview.md)
- [GPU passthrough](docs/gpu-passthrough.md) · [GPU via DRA](docs/gpu/dra-allocation.md)
- [Networking operations](docs/networking/operations-guide.md)
- [Snapshots](docs/snapshots/csi-snapshots.md) · [Live migration](docs/migration/overview.md)
- [Sandboxes](docs/sandbox/overview.md) — ephemeral OCI-rootfs microVMs
- [swiftctl CLI](docs/swiftctl.md) · [Observability](docs/observability/README.md)
- [Install (Helm/OCI)](docs/install/helm-oci.md)

Questions or problems: [GitHub Issues](https://github.com/kubeswift-io/kubeswift/issues).

## Build

```bash
make build          # Go binaries
make build-images   # container images
make deploy         # apply CRDs + deploy the controller
go test ./...       # Go tests
cargo test          # Rust tests (from rust/)
```

## Status

Pre-1.0; the `v1alpha1` API may change between releases.

## License

Licensed under the [GNU Affero General Public License v3.0](LICENSE) (AGPL-3.0).
