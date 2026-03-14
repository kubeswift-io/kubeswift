# KubeSwift

**KubeSwift** runs Linux VMs on Kubernetes using [Cloud Hypervisor](https://www.cloud-hypervisor.org/) as the sole hypervisor—no libvirt, no QEMU. You define guests with Kubernetes custom resources; the control plane reconciles them into pods; a node-side launcher (`swiftletd`) starts Cloud Hypervisor and reports VM state.

## What is KubeSwift?

KubeSwift adds VM orchestration to Kubernetes. Each guest is a pod: Kubernetes schedules it, provides networking and storage, and the launcher container runs Cloud Hypervisor inside the pod. The control plane reconciles four CRDs—**SwiftGuest** (a VM), **SwiftGuestClass** (CPU/memory template), **SwiftImage** (disk source), **SwiftSeedProfile** (cloud-init)—and drives the runtime.

## Why it exists

KubeSwift targets a narrow, modern use case: Linux cloud images, virtio devices, explicit VM semantics. It skips libvirt and multi-hypervisor abstraction. Kubernetes handles orchestration; Cloud Hypervisor handles the VMM. The result is a simpler stack for cloud-native workloads.

## How it differs from KubeVirt

| Aspect | KubeVirt | KubeSwift |
|--------|----------|-----------|
| VMM | libvirt, QEMU, multi-hypervisor | Cloud Hypervisor only |
| API style | VirtualMachine, VirtualMachineInstance | SwiftGuest, SwiftImage, SwiftGuestClass |
| Scope | Broad legacy and cloud support | Linux cloud images, modern virtio |
| Abstraction | VM-as-pod, complex layering | Explicit VM semantics, one guest per pod |

KubeSwift is intentionally distinct: different naming, narrower scope, and a direct Cloud Hypervisor integration.

## Project status

- **Experimental / pre-1.0** — API may change; no stability guarantee
- **Linux-first** — Linux cloud images only; no Windows
- **x86_64-first** — Primary target architecture
- **Cloud Hypervisor only** — No libvirt, no QEMU
- **Current focus:** first-boot, installability, smoke validation

**Planned improvements:**
- Generated API reference tables from Go types / CRDs
- Diagram assets for architecture and lifecycle
- Troubleshooting matrix

**Implemented:** Linux cloud images (raw, qcow2), one root disk, one network, NoCloud seed, start/stop/restart, OCI Helm install, admission webhooks (optional), worker-node preflight, smoke test.

**Not yet implemented:** SwiftGuestMigration, SwiftGuestSnapshot, SwiftGuestPool, ConfigDrive/Ignition seeds, multi-disk, multi-NIC, Windows guests, live migration, snapshots.

## Main components

| Component | Purpose |
|-----------|---------|
| **controller-manager** | SwiftImage, SwiftGuest controllers; optional admission webhooks |
| **swiftletd** | Node daemon; launches Cloud Hypervisor, manages VM lifecycle |
| **Helm chart** | OCI chart at `oci://ghcr.io/projectbeskar/charts/kubeswift` |

## API groups and main CRDs

| API group | CRD | Purpose |
|-----------|-----|---------|
| `swift.kubeswift.io` | SwiftGuest | A single VM instance |
| `swift.kubeswift.io` | SwiftGuestClass | Cluster-scoped template (CPU, memory, root disk) |
| `image.kubeswift.io` | SwiftImage | Disk image source (HTTP, PVC) |
| `seed.kubeswift.io` | SwiftSeedProfile | Cloud-init datasource (NoCloud) |

See [API overview](docs/api/overview.md) and [architecture](docs/architecture.md).

## Quick install

**Remote cluster (Helm OCI):**

```bash
helm install kubeswift oci://ghcr.io/projectbeskar/charts/kubeswift \
  --version 0.1.0 \
  -n kubeswift-system \
  --create-namespace
```

**Local cluster (kind/minikube):**

```bash
make build-images
make load-images   # loads images into kind/minikube
make deploy
```

Version guide: dev `0.0.0-dev.<shortsha>`, RC `X.Y.Z-rc.N`, stable `X.Y.Z`. See [install docs](docs/install/).

## Quick smoke test

After install, run:

```bash
make smoke-test
```

Or manually: apply samples from `config/samples/`, wait for SwiftImage Ready (5–15 min), then SwiftGuest Running. See [smoke verification](docs/operator/smoke-verification.md#quick-walkthrough).

## Repo layout summary

```
api/          # Go API types (swift, image, seed)
cmd/          # controller-manager, swiftctl
config/       # CRDs, RBAC, Kustomize, samples
charts/       # Helm chart (OCI)
images/       # Containerfiles (controller-manager, swiftletd)
rust/         # swiftletd, swift-runtime, swift-seed, swift-ch-client
hack/         # version.sh, chart-version.sh
docs/         # Documentation
```

See [repo layout](docs/developer/repo-layout.md).

## Documentation

**[docs/index.md](docs/index.md)** — Full index

| Topic | Docs |
|-------|------|
| Architecture | [Overview](docs/architecture.md), [Control plane](docs/architecture/control-plane.md), [Node runtime](docs/architecture/node-runtime.md) |
| Install | [Local cluster](docs/install/local-cluster.md), [Remote cluster](docs/install/remote-cluster.md), [Helm OCI](docs/install/helm-oci.md) |
| API | [Overview](docs/api/overview.md), [SwiftGuest](docs/api/swiftguest.md), [SwiftGuestClass](docs/api/swiftguestclass.md), [SwiftImage](docs/api/swiftimage.md), [SwiftSeedProfile](docs/api/swiftseedprofile.md) |
| Operator | [Preflight](docs/operator/worker-node-preflight.md), [Checklist](docs/operator/operator-checklist-ubuntu-x86_64.md), [Smoke test](docs/operator/smoke-verification.md), [Troubleshooting](docs/operator/troubleshooting.md) |
| Developer | [Getting started](docs/developer/getting-started.md), [Build](docs/developer/build.md), [Repo layout](docs/developer/repo-layout.md), [Testing](docs/developer/testing.md) |
| Releases | [Versioning, dev/rc/stable](docs/releases.md) |

## Release model

- **Dev:** Push to `main` → images `sha-<shortsha>`, chart `0.0.0-dev.<shortsha>`
- **RC:** Tag `v*.*.*-rc.*` → images and chart with tag version
- **Stable:** Tag `v*.*.*` → images, chart, GitHub Release

Workflows: `release-dev.yaml`, `release-rc.yaml`, `release-stable.yaml`. See [releases](docs/releases.md).

## Current limitations

- Linux cloud images only; no Windows
- One root disk, one network per guest
- NoCloud seed only; ConfigDrive/Ignition not implemented
- No live migration, snapshots, or SwiftGuestPool
- Worker nodes require KVM and `/dev/kvm`; run preflight before use

## Contributing

Clone, build with `make build`, deploy locally with `make deploy`. See [developer getting started](docs/developer/getting-started.md).
