# KubeSwift

Kubernetes-native VM runtime built on Cloud Hypervisor (+ QEMU for GPU workloads).
VMs are first-class Kubernetes workloads via CRDs. Not Kata Containers — this is a VM platform.

## Key Documents

- @kubeswift_context.md — canonical project context, architecture, CRDs, bugs, roadmap
- @swiftgpu_design_sketch.md — SwiftGPU CRD types, RuntimeIntent extensions, QEMU launcher design
- @kubeswift_architecture.rtf — architecture reference

Read kubeswift_context.md before starting any work.

## Agent Team

Use these agents for specialized review and implementation:

- `/staff-architect` — architectural decisions, CRD design, cross-component coordination, design principle enforcement
- `/rust-runtime-engineer` — all Rust work: swiftletd, swift-qemu-client, QMP, QEMU command generation
- `/security-engineer` — privilege review, VFIO isolation, RBAC audit, container security contexts (review only, no edits)
- `/qa-engineer` — test design, mock GPU data, smoke tests, hardware-free validation, QEMU arg verification

## Languages & Structure

- **Go**: controllers, CRD types, CLI (`api/`, `internal/`, `cmd/`)
- **Rust**: VM launcher, runtime (`rust/swiftletd/`, `rust/swift-ch-client/`, `rust/swift-qemu-client/`)
- **Shell**: init containers, entrypoints (`images/swiftletd/scripts/`)
- **Helm**: chart at `charts/kubeswift/`

## Build & Test Commands

```bash
make build                    # build Go binaries
make build-images             # build container images
make push-images              # push to ghcr.io
make deploy                   # CRD apply + controller deploy
make generate                 # controller-gen CRD regeneration
make smoke-test               # end-to-end boot test
cargo build --release         # build Rust crates (from rust/ workspace)
cargo test                    # run Rust tests
go test ./...                 # run Go tests
```

## CRD Workflow

After ANY change to Go types in `api/`:
```bash
make generate
cp config/crd/bases/*.yaml charts/kubeswift/crds/
make deploy
```
The API server **silently drops** unknown fields if CRDs drift from Go types.

## Critical Rules

### Architecture
- Cloud Hypervisor is the DEFAULT runtime — QEMU only when GPU hardware requires it
- Runtime disks must be **raw** — qcow2 is input format only
- rust-hypervisor-firmware loads via `--kernel`, NEVER `--firmware`
- `--firmware` is for OVMF/EDK2 only (QEMU path)
- `imageRef` and `kernelRef` are mutually exclusive on SwiftGuest
- `gpuProfileRef` can combine with `imageRef` but NOT with `kernelRef`
- Working guest OS (disk boot): **Ubuntu Focal 20.04** — do NOT use Noble
- RestartPolicy on launcher pods is ALWAYS `Never`

### Status Reporting
- swiftletd reports via **pod annotations**, not direct SwiftGuest status patches
- Controller reads annotations on reconcile and maps to SwiftGuest status
- GuestRunning condition is the one exception — patched via kube-rs DynamicObject

### Networking
- `eth0` is NOT enslaved to `br0` (Bug 1 — broke pod networking)
- `br0` gets its own IP (10.244.125.1) as gateway
- launcher-entrypoint.sh starts dnsmasq when `"network": true` in intent JSON
- QEMU path uses same tap0/br0 model via `-netdev tap,ifname=tap0`

### GPU (SwiftGPU)
- Tier 1 PCIe GPUs: Cloud Hypervisor + `x_nv_gpudirect_clique`
- Tier 2/3 HGX SXM GPUs: QEMU + `pcie-root-port` per device (CUDA refuses flat topology)
- Large BARs (>64GB): `x-no-mmap=true` on QEMU to avoid boot stalls
- Fabric Manager host version must exactly match guest nvidia-open driver version
- gpu-init container handles VFIO bind + FM partition activate BEFORE swiftletd starts
- QEMU uses QMP (unix socket) for monitoring, not HTTP API like CH
- GPU node opt-in label: `kubeswift.io/gpu-node=true`

### SwiftKernel
- Node opt-in label: `kubeswift.io/kernel-node=true`
- Kernel artifact path: `/var/lib/kubeswift/kernels/<namespace>-<name>/`
- Production kernel tag: `6.6.1` — do NOT use `6.6.0` (broken networking)
- ORAS version: `ghcr.io/oras-project/oras:v1.3.1`

## Code Style

### Go
- Use controller-runtime patterns (Reconcile loop, conditions, status subresource)
- CRD marker comments must include `+kubebuilder:` annotations
- All new API groups need scheme registration in `internal/scheme/`

### Rust
- Workspace at `rust/Cargo.toml` — add new crates as workspace members
- Async runtime: tokio
- Error handling: anyhow for applications, thiserror for libraries
- JSON: serde + serde_json (use `serde_json::json!` not `format!` for annotations)
- Kubernetes client: kube-rs with DynamicObject for status patches

## Development Principles

1. **Minimal changes** — one fix at a time, verified with real cluster output
2. **No speculative fixes** — diagnose with actual logs before patching
3. **No silent failures** — status fields must reflect real system state
4. **Verified before merged** — never assume a fix worked without confirming output

## Current Work: SwiftGPU

Active development track. See @swiftgpu_design_sketch.md for full design.

Phase 1: QEMU runtime path in swiftletd (no GPU hardware needed)
Phase 2: GPU passthrough Tier 1 — PCIe GPUs on Cloud Hypervisor
Phase 3: GPU passthrough Tier 2 — HGX SXM on QEMU with PCIe topology
Phase 4: Full PCIe hierarchy for Tier 3 HGX full passthrough
