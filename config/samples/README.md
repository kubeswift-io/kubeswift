# KubeSwift Sample Manifests

Sample manifests organized by scenario. Most directories include a README with
prerequisites, apply order, and expected results.

All seed profiles include `ssh_authorized_keys` — replace with your own key.

## Shared resources

Used across multiple scenarios. Apply these first.

```bash
kubectl apply -f config/samples/shared/swiftguestclass-default.yaml
kubectl apply -f config/samples/shared/swiftseedprofile-minimal.yaml
```

## Quick start (disk boot)

```bash
kubectl apply -k config/rbac
kubectl apply -f config/samples/shared/swiftguestclass-default.yaml
kubectl apply -f config/samples/shared/swiftseedprofile-minimal.yaml
kubectl apply -f config/samples/disk-boot/swiftimage-ubuntu-noble.yaml
kubectl get swiftimage ubuntu-noble -w  # wait for Ready (5-15 min)
kubectl apply -f config/samples/disk-boot/swiftguest-sample.yaml
kubectl get swiftguest sample -w        # wait for Running + IP
swiftctl console sample                 # serial console
swiftctl ssh sample -u kubeswift        # SSH access
```

## Catalog

### Boot

| Directory | Description |
|-----------|-------------|
| [shared/](shared/) | Default SwiftGuestClass + minimal SwiftSeedProfile |
| [disk-boot/](disk-boot/) | Ubuntu Noble 24.04 cloud-image boot (Cloud Hypervisor) |
| [kernel-boot/](kernel-boot/) | faas-minimal direct kernel boot from an OCI artifact |
| [qemu-boot/](qemu-boot/) | Ubuntu Noble via QEMU/OVMF |
| [rocky/](rocky/) | Rocky Linux 9 disk boot |
| [windows/](windows/) | Windows guest (`osType: windows`, Cloud Hypervisor) |

### GPU

| Directory | Description |
|-----------|-------------|
| [gpu-pcie/](gpu-pcie/) | Tier 1 PCIe GPU passthrough (Cloud Hypervisor) — GTX 1080, A100-PCIe |
| [gpu-hgx/](gpu-hgx/) | Tier 2 HGX SXM shared-NVSwitch profile (QEMU) |
| [dra-gpu/](dra-gpu/) | GPU passthrough via DRA ResourceClaims (scheduler-allocated) |

### Storage & disks

| Directory | Description |
|-----------|-------------|
| [datadisk/](datadisk/) | Secondary data disks: blank/sized, image-backed, attached PVC |
| [storage/](storage/) | Access-mode selection: RWO+Filesystem, RWX+Block (migratable) |

### Networking

| Directory | Description |
|-----------|-------------|
| [multi-nic/](multi-nic/) | Multi-NIC via Multus: bridge, VLAN, OVN L2/L3/localnet, CUDN/UDN NADs |
| [multi-node-l2/](multi-node-l2/) | Primary-on-NAD flat L2 and kube-ovn IP-preserving guests |
| [sriov/](sriov/) | SR-IOV NIC passthrough (VFIO) for GPUDirect RDMA / DPDK |
| [service-exposure/](service-exposure/) | Expose guest ports as Kubernetes Services (in-pod DNAT) |
| [model-a/](model-a/) | Guest on a namespace primary UDN (tenant isolation) |

### Fleets

| Directory | Description |
|-----------|-------------|
| [pool/](pool/) | SwiftGuestPool: basic, spread, rolling-update, stateful, load-balanced, GPU inference |

### Snapshots, clones & migration

| Directory | Description |
|-----------|-------------|
| [local-snapshots/](local-snapshots/) | Tier B local memory+disk snapshots; in-place and clone restore |
| [s3-snapshots/](s3-snapshots/) | Tier C snapshots on S3 object storage (MinIO); cross-node clone |
| [oci-snapshots/](oci-snapshots/) | Snapshots pushed to an OCI registry (`backend.type: oci`); underpins cold migration |
| [clone-from-snapshot/](clone-from-snapshot/) | `cloneFromSnapshot`: fan out a pool from one snapshot |
| [snapshot-schedule/](snapshot-schedule/) | Cron-scheduled snapshots + keep-N retention (CSI, S3+TTL) |
| [snapshots-walkthrough/](snapshots-walkthrough/) | Six numbered scenarios exercised in the operator walkthrough |
| [migration/](migration/) | SwiftMigration: basic offline, allow-IP-change, drain (phase-4) |
| [migratable-guests/](migratable-guests/) | Guests configured for clean live migration (RWX+Block) |

### Registry

| Directory | Description |
|-----------|-------------|
| [golden-image/](golden-image/) | Golden VM image from an OCI registry (`SwiftImage.spec.source.oci`) |
| [edge-zot/](edge-zot/) | Per-site Zot mirroring VM artifacts from a hub |

### Gateway & seeds

| Directory | Description |
|-----------|-------------|
| [gateway/](gateway/) | Fleet `Cluster` objects + explorer/member RBAC for the gateway |
| [seed-profiles/](seed-profiles/) | cloud-init seed profiles: guest-agent, clone-identity regen |

### Other

| Directory | Description |
|-----------|-------------|
| [security/](security/) | SwiftGuestClass with vCPU core-scheduling (SMT side-channel mitigation) |
| [monitoring/](monitoring/) | kube-state-metrics CustomResourceState config for KubeSwift CRs |
| [vhost-user-net/](vhost-user-net/) | vhost-user-net device |
| [vhost-user-devices/](vhost-user-devices/) | vhost-user-blk and generic `--user-device` passthrough |
| [virtiofs/](virtiofs/) | virtiofs shared filesystem (hostPath, read-only PVC) |
| [advanced/](advanced/) | PVC-clone image source, SSH and Secret-backed seed profiles |
