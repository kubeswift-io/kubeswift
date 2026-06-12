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

---

# Addendum (2026-06-12): P2 without datacenter hardware — the reference driver plan

> Status: **APPROVED — in implementation.** Supersedes the original "P2 is
> hardware-gated" framing: most of P2 is buildable and **cluster-validatable
> today**. Two facts changed the calculus: the dev cluster runs **k8s
> v1.34.3+k0s with `resource.k8s.io/v1` served** (real DRA scheduling, no
> upgrade), and the hardware gate was really an *NVIDIA-driver* gate — boba's
> VFIO-proven GTX 1080 is sufficient for the whole passthrough path if
> **KubeSwift ships its own minimal reference DRA driver**.

## A1. What stays gated vs. what un-gates

| Was "P2, hardware-gated" | Now |
|---|---|
| Claim-bearing unpinned launcher pod | **Buildable now** (Workstream A) |
| Post-schedule device identity → gpu-init/swiftletd | **Buildable now** (A — the CDI-env design below) |
| Pin the `AllocatedDeviceStatus.Data` schema | **We pin our own reference schema now** (B); the NVIDIA driver becomes a later schema *adapter*, not an architecture unknown |
| gpu-init-vs-driver binding ownership | **Decided for the reference driver** (driver does NOT bind; gpu-init keeps the proven two-pass bind). NVIDIA-mode ownership remains a later question |
| Boot one VM end-to-end via DRA | **Cluster-validatable now** on the GTX 1080 (C) |
| ComputeDomains/IMEX (P3), MIG (P4), QEMU/HGX tiers under DRA | Still hardware-gated |

## A2. The device hand-off — CDI-injected env (the load-bearing decision)

The inversion's hard runtime problem: in the native flow devices are known
*before* the pod exists (env + intent CM at build time); in DRA the pod exists
*first*. Env is immutable and init containers run before any controller
round-trip can be trusted (a `Resolve`→annotation→downward-API loop would race
`gpu-init`).

**Decision: the kubelet/CDI layer carries the device identity.** Our reference
driver's `NodePrepareResources` returns a CDI device whose `containerEdits`
inject **`GPU_PCI_ADDRESSES=<bdf>[,<bdf>…]`** (and `GPU_PARTITION_ID=-1`) into
every container that references the claim. Properties:

- **`gpu-init.sh` runs unchanged** — it already consumes exactly these envs.
- **No race**: CDI edits are applied by the container runtime at
  container-create, atomically before the init container starts.
- **`Resolve` becomes status-only**: it stamps `status.GPU` for the control
  plane (observability, `kubectl get sg`, capacity views) but is NOT
  load-bearing for the pod runtime — eliminating risk-class "controller
  round-trip vs init container" entirely.
- The CDI device is **env-only** (no `deviceNodes` edits): the launcher pod
  already hostPath-mounts `/dev/vfio` and `gpu-init` performs the proven
  idempotent two-pass vfio-pci bind. The driver advertises + allocates +
  hands identity over; binding stays where it is hardened. (Driver-side
  binding — NVIDIA VFIO-mode parity — is a follow-up, noted in A6.)

swiftletd side: in DRA mode the controller writes the intent with
`gpu.deviceSource: "env"` and an empty device list (plus firmware/hugepages
from `GPUResourceClaimSpec`); swiftletd synthesizes the `VFIODeviceIntent`s
from `GPU_PCI_ADDRESSES` (clique −1, NUMA 0 in v1 — explicit marker, no silent
magic).

## A3. Device naming pins the read-back — independent of beta features

`extractDeviceBDFs` (the P1 isolation point) gets a **two-tier contract**:

1. **Preferred:** `AllocatedDeviceStatus.Data: {"pciAddress": "<bdf>"}` —
   written by our driver; the same field an external driver's adapter would
   populate. Requires the `DRAResourceClaimDeviceStatus` feature (beta) —
   verified on-cluster in Workstream C.
2. **Fallback (always works):** the ResourceSlice **device name encodes the
   BDF** — `gpu-0000-01-00-0` ⇄ `0000:01:00.0` (DNS-label-safe). The
   allocation result's `Driver/Pool/Device` triple is GA API; if the status
   write is unavailable, `Resolve` decodes the name. Our naming scheme is part
   of the reference-driver contract.

## A4. Workstream B — `cmd/kubeswift-dra-driver` (reference driver)

A DaemonSet on `kubeswift.io/gpu-node=true` nodes (same opt-in as
gpu-discovery), driver name **`gpu.kubeswift.io`**:

- **Inventory → ResourceSlice**: reuses the gpu-discovery sysfs logic
  (BDF/NUMA/IOMMU group/vfioReady) to publish one ResourceSlice per node;
  device attributes: `pciAddress`, `model`, `numaNode`, `iommuGroup`,
  `vfioReady`. With **structured parameters the SCHEDULER allocates** — the
  driver has no allocation controller at all.
- **Kubelet plugin** via `k8s.io/dynamic-resource-allocation/kubeletplugin`
  (v0.35.x, matches our deps): registration socket under
  `/var/lib/kubelet/plugins_registry` (verified: k0s uses the **standard**
  kubelet root `/var/lib/kubelet`), `NodePrepareResources` writes the CDI spec
  (env-only, §A2) to `/var/run/cdi` and returns the CDI device IDs;
  `NodeUnprepareResources` removes it.
- **Claim device status**: best-effort write of
  `status.devices[].data{pciAddress}` (§A3 tier 1).
- **DeviceClass sample**: `kubeswift-vfio-gpu` selecting
  `device.driver == "gpu.kubeswift.io"`.
- Strategic note: KubeVirt's DRA driver is a POC — this is a working
  VM-passthrough reference driver; pioneering, deliberately minimal.

## A5. Cluster prerequisites (Workstream C node prep)

Verified on the dev cluster (k0s 1.34.3, containerd 1.7.30):

- **CDI is OFF by default** in containerd 1.7 (`enable_cdi = false` in the
  k0s-generated `/run/k0s/containerd-cri.toml`). Enable via a k0s containerd
  import drop-in (`/etc/k0s/containerd.d/99-cdi.toml` →
  `[plugins."io.containerd.grpc.v1.cri"] enable_cdi = true` +
  `cdi_spec_dirs`) and restart `k0sworker`. One-time per GPU node — same
  class of host prerequisite as persistent `vfio-pci` loading.
- kubelet plugin dirs are the standard `/var/lib/kubelet/{plugins,plugins_registry}`.
- `vfio-pci` module persistence on GPU nodes (pre-existing requirement).

## A6. Phasing (this arc) and the residual gate

- **A — runtime plumbing (Go+Rust, unit/envtest):** unpinned claim-bearing
  GPU pod (`pod.spec.resourceClaims` + `resources.claims` on gpu-init +
  launcher, no GPU env at build, no nodeSelector); DRA branch of
  `buildGPUIntent` (from `GPUResourceClaimSpec`, no profile/SwiftGPUNode);
  `gpu.deviceSource: env` intent marker + swiftletd env synthesis; lift the
  `GPUDRARuntimePending` hold; `Resolve` reads name-encoded BDFs as fallback.
- **B — the reference driver** (§A4) + image + manifests.
- **C — cluster e2e:** node prep (§A5), deploy driver, DeviceClass +
  ResourceClaimTemplate samples, a SwiftGuest with `gpuResourceClaim` gets the
  **GTX 1080 via a real scheduler allocation**, VM boots, `status.GPU`
  read-back validated; walkthrough doc.

**Residual hardware gate after this arc:** the NVIDIA `k8s-dra-driver-gpu`'s
actual `Data`/CDI schema (an adapter in `extractDeviceBDFs` + possibly a
binding-ownership switch in gpu-init), ComputeDomains/IMEX (P3), MIG (P4),
QEMU/HGX tiers under DRA. Everything else moves to "validated".

## A7. Addendum risks

1. **CDI enablement is a node-config change** (containerd restart) — staged on
   boba first, documented as a GPU-node prerequisite.
2. **`DRAResourceClaimDeviceStatus` may be unavailable** — covered by the
   name-encoding fallback (§A3); the walkthrough records which tier ran.
3. **kubeletplugin helper API churn** between k8s minors — pin v0.35.x; the
   driver is small enough to track.
4. **Driver crash during NodePrepareResources** blocks pod start — kubelet
   retries; DaemonSet restart recovers; document the failure surface.
5. **The reference schema is ours, not NVIDIA's** — explicitly a *reference*;
   `extractDeviceBDFs` stays the single adapter point (P1 risk #2 unchanged).
