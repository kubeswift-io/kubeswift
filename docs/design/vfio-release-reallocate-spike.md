# VFIO Release-and-Reallocate — Spike Findings

> Spike for the Phase 4 drain follow-on sub-phase (TFU #27): the SwiftGPU
> primitive that lets a VFIO/GPU guest be evacuated off a node (offline
> migration). Run 2026-06-02 on the dev cluster (boba, GTX 1080).
> Decisions going in: **migration controller orchestrates** (SwiftGPU
> exposes reserve/release primitives); **spike first** to de-risk the
> dealloc->realloc choreography on real hardware before designing.

## Goal

Validate the core unknown: can the real GTX 1080 be **released** by one guest
and **reacquired** by a fresh guest on the same node, cleanly, with the new
guest booting? The cluster has one GPU node, so cross-node is mocked/unit-
tested per the validation plan; this spike validates the same-node
release->reacquire choreography on hardware.

## Result: PASS (after clearing two prerequisite blockers)

| Step | Result |
|---|---|
| GPU guest boots (baseline) | ✅ after the two fixes below |
| **Release** (delete guest -> finalizer dealloc) | ✅ GPU freed in ~3s; `SwiftGPUNode` `free=1`, `allocated=false` |
| **Reacquire** (fresh guest allocates the SAME GPU) | ✅ guest #2 got `0000:01:00.0`, booted to Running (~80s incl. image clone) |
| GPU stays vfio-bound across the cycle | ✅ guest #2's gpu-init saw "already bound to vfio-pci" |

**The dealloc->realloc choreography is sound on real hardware.** The
release-and-reallocate primitive is de-risked: a guest releases the GPU on
exit (CH closes the vfio device; the device stays vfio-pci-bound), and a new
guest's gpu-init idempotently re-confirms the bind and CH re-opens it.

## Two prerequisite blockers surfaced (and how they were cleared)

### Blocker 1 — gpu-init IOMMU-group bind order (FIXED, PR #93)

gpu-init bound IOMMU-group **peers** (the GTX 1080's HD-Audio function
`0000:01:00.1`) to vfio-pci **before** unbinding the GPU itself from its host
driver. vfio-pci's viability check refuses to bind a device while another
device in the same IOMMU group is on a non-vfio driver, so with the GPU on
`nvidia` the peer bind failed (`bound to '', expected 'vfio-pci'`) and
gpu-init exited 1 (`Init:Error`). Fixed with the standard two-pass procedure
(unbind **all** group devices, then bind all). Validated on hardware here:
gpu-init now logs `Unbinding 0000:01:00.0 from nvidia` then binds the GPU and
the audio peer to vfio-pci and completes.

### Blocker 2 — `vfio-pci` module not loaded on the GPU node (HOST PREREQUISITE)

Even with the bind order fixed, the GPU bind failed (`bound to ''`) because
boba had **no `vfio-pci` module loaded** (`/proc/modules`: no vfio; no
`/sys/bus/pci/drivers/vfio-pci`). With nothing registered as vfio-pci,
`drivers_probe` cannot bind any device. Loading it (`modprobe vfio-pci`, which
pulls `vfio`, `vfio_pci_core`, `vfio_iommu_type1`) made the GPU guest boot.

The original Tier-1 validation must have had vfio-pci loaded; it was lost
(reboot without persistent module config). **This is a host prerequisite for
GPU nodes**, and a design item for the sub-phase:

- **Operator prerequisite (minimum):** GPU nodes (`kubeswift.io/gpu-node=true`)
  must load `vfio-pci` persistently — e.g. `/etc/modules-load.d/vfio.conf`
  with `vfio-pci`, or the kernel cmdline. Document in the GPU operator guide.
- **Candidate kubeswift mechanism (design open question):** the gpu-discovery
  DaemonSet (already privileged, already per-GPU-node) could `modprobe
  vfio-pci` and surface a `vfio-pci`-loaded condition on `SwiftGPUNode.status`,
  turning a silent `Init:Error` into a clear node-readiness signal. gpu-init
  itself should NOT load the module (it runs with minimal capabilities, not
  privileged; loading a kernel module needs `CAP_SYS_MODULE`).
- **Pre-flight gate:** the SwiftGPU allocation (and the future migration GPU
  target pre-flight) should refuse a node whose `vfio-pci` is not loaded,
  rather than allocate and let gpu-init fail.

## Implications for the release-and-reallocate design

1. **Reserve/release primitives are viable.** Dealloc frees the GPU cleanly
   (~3s) and a fresh guest reacquires the same device. The migration
   controller can drive: reserve target GPUs -> stop source -> release source
   GPUs -> commit `status.GPU.NodeName` -> recreate on target.
2. **The GPU stays vfio-bound between guests.** No host driver re-grab happens
   in the release->reacquire window (CH closes the device but leaves it
   vfio-pci-bound; gpu-init is idempotent). So the realloc'd pod's gpu-init is
   a fast no-op on the bind, not a full rebind — lowers the per-migration GPU
   cost.
3. **Node readiness (vfio-pci) is part of GPU target selection.** The GPU
   analogue of `NodeHasCapacity` must also confirm the target node is
   vfio-ready (vfio-pci loaded), not just that it has free matching GPUs.
4. **The precedence rule must change.** Today the GPU pod builder
   ([`gpu.go`](../../internal/controller/swiftguest/gpu.go)) hard-rejects
   `spec.NodeName != status.GPU.NodeName`. Release-and-reallocate updates both
   together (release on S, reallocate on T, flip `status.GPU.NodeName=T`); the
   rule becomes "they must agree at pod-build time", which the orchestrated
   sequence guarantees.
5. **Cross-node remains hardware-unvalidated** (one GPU node). This spike
   covers the same-node choreography; the cross-node target-selection +
   FM-partition-handoff (Tier 2/3) stay unit/envtest-only with a mocked second
   `SwiftGPUNode`, shipped explicitly labeled.

## Cluster state after the spike

All spike guests/PVCs deleted; GPU free (`free=1`). The launcher image is
`swiftletd:sha-df40efe` (carries the PR #93 gpu-init fix). **`vfio-pci` was
loaded manually on boba and is NOT persistent** — it will be lost on a boba
reboot until a persistent host config or a kubeswift mechanism (above) lands.
