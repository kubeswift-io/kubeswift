# DRA / NVIDIA-ecosystem GPU integration for KubeSwift

> RFC + Phase 1 implementation. Status: **Phase 1 (allocation backend) IMPLEMENTED**;
> the DRA passthrough runtime is **Phase 2 (hardware-gated)**.
> See also `swiftgpu_design_sketch.md`, `docs/gpu-passthrough.md`.

## 1. Why

KubeSwift handles GPUs with a fully custom subsystem: the `gpu-discovery`
DaemonSet + `kubeswift.io/gpu-node` labels, the `SwiftGPUProfile` /
`SwiftGPUNode` CRDs, and a controller (`findAndAllocate`) that picks node+devices
and VFIO-passes whole GPUs into VMs. This is excellent for a **heterogeneous
estate** and works with zero NVIDIA cluster software — but it is bespoke and
cannot express what NeoCloud / AI-distributed-inference operators increasingly
need: **standard Kubernetes allocation APIs**, **multi-node NVLink/IMEX**
(GB200 NVL72), and **MIG/fractional** GPUs.

The ecosystem moved: **DRA is GA** (Kubernetes 1.34, locked-in 1.35); the
**NVIDIA DRA driver** (`k8s-dra-driver-gpu`, now CNCF) does full-GPU, MIG,
sharing, **ComputeDomains/IMEX**, and gained **VFIO/IOMMUFD passthrough**. But
DRA-for-VM-passthrough has **no standard pattern yet** (KubeVirt's is a POC), so
KubeSwift pioneers the `ResourceClaim → VFIO → VM` glue.

**Decision: hybrid/pluggable.** DRA is an *opt-in* allocation backend; the
validated native SwiftGPU model stays the default. This positions KubeSwift for
NeoCloud/AI-inference without abandoning today's strength.

## 2. The key insight — two separable layers

KubeSwift's GPU subsystem is two layers coupled by exactly one struct,
`SwiftGuest.Status.GPU`:

1. **Allocation + discovery** — *which* GPUs, on *which* node. **DRA replaces/augments this.**
2. **VM-passthrough translation + runtime** — given allocated PCI BDFs, bind to
   `vfio-pci` and pass into the VM (`buildGPUIntent` → `VFIODeviceIntent` →
   `gpu-init` → CH/QEMU `--device`). **Reused unchanged.**

Anything that populates `Status.GPU` correctly is a valid backend.

## 3. The control-flow inversion

| | Native | DRA |
|---|---|---|
| When is allocation decided? | In the **controller**, before the pod exists (`findAndAllocate`). | At **scheduler** time — the pod carries a `ResourceClaim`; the scheduler + DRA driver pick node+device. |
| Node pinning | Controller pins the pod to the chosen node. | Scheduler picks the node (pod unpinned). |
| KubeSwift's job | Decide, then build a pinned pod. | Build a claim-bearing pod, then **read the result back**. |

A single `Allocate()` call cannot express both. The backend interface is
therefore **two-phase**: `Prepare` (controller-time) then `Resolve`
(after the pod is scheduled).

## 4. The pluggable backend (`internal/gpualloc`)

```go
type Backend interface {
    Name() string
    Prepare(ctx, guest) (*PrepareResult, error)  // native: decide now; dra: defer
    Resolve(ctx, guest) (*Resolution, error)      // native: no-op; dra: read claim back
    Release(ctx, guest) error                     // finalizer path
}
```

- **`nativeBackend`** (`internal/controller/swiftgpu/native_backend.go`) is a thin
  wrapper that **moves no allocation logic** — `findAndAllocate`, `selectGPUs`,
  `findFMPartition`, `deallocateGPUs`, `ReserveOnNode`, `ReleaseFromNode` are
  untouched. `Prepare` returns `{Resolved: true, Status}`; `Resolve` is a no-op.
- **`DRABackend`** (`internal/gpualloc/dra.go`): `Prepare` returns a `PodBinding`
  (the claim reference, no node/devices); `Resolve` finds the scheduled launcher
  pod, reads `ResourceClaim.Status.Allocation` + `AllocatedDeviceStatus.Data`,
  and maps the device to a PCI BDF + the scheduled node → `GPUStatus`.

The `SwiftGPUReconciler` selects the backend from `guest.GPUBackend()`
(`gpuProfileRef → native`, `gpuResourceClaim → dra`) and runs
`Prepare → (commit or defer) → Resolve`. The native path's
ProfileNotFound/NoCapacity conditions, requeues and metrics are byte-preserved
(mapped from typed errors). The DRA deferred path sets a `GPUClaimPending`
condition and polls `Resolve`.

The **driver-specific `Data` schema is isolated in one function**
(`extractDeviceBDFs`): the NVIDIA driver owns the RawExtension shape; only that
function changes when P2 pins it.

## 5. API surface

```yaml
spec:
  gpuResourceClaim:                 # opt into the DRA backend (XOR gpuProfileRef)
    resourceClaimTemplateName: gpu  # or resourceClaimName (exactly one)
    tier: pcie                      # pcie -> Cloud Hypervisor, hgx-* -> QEMU
    hugepages: "1Gi"
```

`gpuResourceClaim` is mutually exclusive with `gpuProfileRef`, `kernelRef`,
`cloneFromSnapshot`, `osType: windows`, and virtiofs/vhost-user — the same
constraint family as `gpuProfileRef` (factored behind `usesGPU(spec)` in the
webhook). `HasVFIODevices()` now covers both backends (DRA GPUs are still
VFIO → offline-migration-only, live-migration-blocked).

## 6. Phasing and the P1/P2 boundary

**Phase 1 (this change — buildable + unit/envtest, NO hardware):** the
*allocation* layer. The pluggable backend, the native refactor
(behavior-preserving), the DRA backend skeleton (read-back proven by a
synthetic-`ResourceClaim` unit test), the `spec.gpuResourceClaim` API, the
webhook, scheme + RBAC. A DRA guest is admitted and the allocation backend runs,
but the swiftguest controller **holds it `Pending`** with a `GPUDRARuntimePending`
condition so it never boots GPU-less — because:

**Phase 2 (hardware-gated) is the DRA RUNTIME:** building the claim-bearing,
unpinned launcher pod; consuming the driver-allocated device (CDI / `gpu-init`)
into the VM; and the `buildGPUIntent`/pod-builder feed for DRA-allocated devices.
This needs a real `k8s-dra-driver-gpu` cluster to validate the device `Data`
schema and the binding ownership (does the driver bind vfio-pci, or `gpu-init`?).
Doing half the runtime without that hardware would be a bridge to nowhere; the
clean split is allocation now, runtime when a GPU lands.

**Later phases (also hardware-gated):**
- **P3 — ComputeDomains/IMEX** (multi-node NVLink, GB200) — DRA-only; the native
  model cannot express it.
- **P4 — MIG/fractional:** a MIG instance is **not** a PCI BDF — needs a new
  `runtimeintent` mediated-device variant + CH/QEMU args.
- **P5 — NFD / GPU-Operator vfio-manager:** once DRA+NFD provide inventory,
  retire parts of `cmd/gpu-discovery` (and possibly `SwiftGPUNode`) for DRA-only
  clusters.

## 7. Key risks

1. **Offline-migration choreography vs. the inversion.** `gpu_offline.go` +
   `ReserveOnNode` assume controller-time allocation; DRA has none. P1 keeps
   `OfflineGPUMigratable()` **native-only**, so DRA guests are not drain-evacuated
   and don't enter the reserve/cutover path. DRA-aware offline migration is a
   later design.
2. **`Data` schema is driver-owned/unpinned** — isolated in `extractDeviceBDFs`;
   P2 pins it.
3. **Maturity churn** — the seam stays at `GPUStatus`; DRA types never thread
   into Layer 2, containing the blast radius.
4. **Node-partitioning** (vfio-pci vs nvidia driver) — DRA must request a
   VFIO-class `DeviceClass`; mixing modes per node is driver policy (P2).
5. **`SwiftGPUNode`/discovery** stays for native; the DRA path must not
   hard-depend on it.
6. **Two-writer hazard on `status.GPU`** — `Resolve` is idempotent; P1's
   exclusion of DRA from migration keeps it the only DRA writer.

## 8. How the four NeoCloud/AI-inference drivers map

| Driver | Served by | Native can do it? |
|---|---|---|
| Multi-node NVLink / IMEX (distributed inference, GB200) | DRA ComputeDomains (P3) | No |
| Standard API / NeoCloud interop | DRA ResourceClaim/DeviceClass (P1 API; P2 runtime) | No |
| MIG / fractional GPU for VMs | DRA MIG + a mediated-device intent (P4) | No |
| Less custom discovery/binding maintenance | NFD / GPU-Operator vfio-manager (P5) | N/A |

The native model remains the right tool for **heterogeneous estates and clusters
without the NVIDIA stack**; DRA is the path for the GPU-cloud/AI-inference
profile. Hybrid keeps both.
