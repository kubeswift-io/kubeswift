# SwiftGPU Tier 3 — HGX Full Passthrough (full PCIe hierarchy + in-guest NVSwitch/FM)

> Status: DESIGN (pre-spike), **hardware-gated**. Tier 3 (`tier: hgx-full`) passes
> an **entire HGX baseboard** — all GPUs **and** all NVSwitches — to a single VM,
> with the **NVSwitch fabric and Fabric Manager running inside the guest**. The CRD
> surface already exists (SwiftGPU Phases 1–3); this is the **runtime + discovery +
> allocation** design. No HGX hardware on the dev cluster — design-first, validated
> when an HGX host is available. Last updated: 2026-06-08.

## 1. Tier recap

| Tier | `spec.tier` | Hypervisor | Topology | NVSwitch / Fabric Manager |
|---|---|---|---|---|
| 1 | `pcie` | Cloud Hypervisor | flat PCI | none (`x_nv_gpudirect_clique`) |
| 2 | `hgx-shared` | QEMU | `pcie-root-port` per GPU | **host** FM partition (a SLICE of the baseboard) |
| 3 | `hgx-full` | QEMU | **full PCIe hierarchy** | **in-guest** NVSwitches + FM (the WHOLE baseboard) |

Tiers 1 (Hetzner GTX 1080, validated) and the Tier 2 model shipped in
SwiftGPU Phases 1–3. The CRD fields for Tier 3 already exist
(`api/gpu/v1alpha1`): `tier: hgx-full`, `partitionMode: full`,
`fabricManager.runInGuest: true`, `pcieTopology.{rootPortPerDevice,noMmap}`,
`numaTopology`, `vcpuPinning`.

## 2. What Tier 3 adds over Tier 2

Tier 2 (`hgx-shared`) carves the baseboard into **partitions** via the **host**
Fabric Manager and hands a *subset* of GPUs (sharing an NVSwitch slice) to a VM.
Tier 3 (`hgx-full`) is the opposite extreme — **the whole baseboard goes to ONE
VM**:

1. **All GPUs (typically 8) + all NVSwitches (4–6)** are VFIO-passed to a single
   guest.
2. The **full PCIe hierarchy** of the HGX baseboard (the PCIe switches/bridges
   wiring GPUs ↔ NVSwitches) is reconstructed in the guest so CUDA + the NVLink
   fabric see the real topology.
3. **Fabric Manager runs INSIDE the guest** (`runInGuest: true`) — the guest owns
   and initializes the NVSwitch fabric. The **host does NOT run FM** for this
   baseboard (there is no host partition; the baseboard is entirely the guest's).
4. **No FM-partition handoff** (unlike Tier 2): allocation is whole-baseboard, so
   there is nothing to partition/deactivate on the host.

This is the configuration NVIDIA documents for full-baseboard passthrough; it is
what a single large multi-GPU training/inference VM wants.

## 3. The core challenge — the PCIe hierarchy

CUDA + the NVLink/NVSwitch fabric **refuse a flat PCI topology** (the reason Tier
2/3 need QEMU, not CH, today). For Tier 3, the guest must see a PCIe hierarchy
that matches the physical baseboard closely enough that:

- the GPUs and NVSwitches enumerate at the right relative positions,
- NVLink discovery + Fabric Manager can build the routing tables,
- GPUDirect P2P/RDMA works across the fabric.

Two construction strategies (spike to choose):

- **(A) Synthesize the hierarchy** — build a matching tree of QEMU
  `pcie-root-port` + `pcie-pci-bridge`/switch elements and attach each GPU /
  NVSwitch `vfio-pci` at the position mirroring the host topology (read from
  `lspci -t` / sysfs `/sys/bus/pci/devices/*/`). The baseboard's PCIe switches
  themselves are *emulated*; only the GPUs + NVSwitches are passed through.
- **(B) Pass the PCIe switches too** — VFIO-pass the upstream/downstream PCIe
  switch ports of the baseboard so the guest sees the *real* bridges. Heavier
  (more IOMMU groups, ACS/peer constraints) but the most faithful.

Recommendation: **spike (A) first** (emulated switches + passthrough leaf
devices, the lighter path) and fall back to (B) if Fabric Manager rejects the
synthesized topology.

## 4. CRD surface (already present)

No new CRD is needed — Tier 3 uses the existing `SwiftGPUProfile`:

```yaml
apiVersion: gpu.kubeswift.io/v1alpha1
kind: SwiftGPUProfile
spec:
  count: 8
  model: H200-SXM
  tier: hgx-full                 # -> QEMU + full PCIe hierarchy
  partitionMode: full            # whole baseboard to one VM
  pcieTopology:
    rootPortPerDevice: true
    noMmap: true                 # large BARs (B200) — x-no-mmap=true
  numaTopology: { sockets: 2, coresPerSocket: 48, memoryPerSocketMi: 983040 }
  vcpuPinning: true
  fabricManager:
    runInGuest: true             # FM in the guest; no host partition
    requiredVersion: "<nvidia-open driver version>"   # guest image driver/FM match
```

The only CRD-adjacent work is a webhook tightening: `tier: hgx-full` ⇒
`partitionMode: full` and `fabricManager.runInGuest: true` (reject the
nonsensical combos), and `count` must equal a discovered baseboard's GPU count
(you can't take *part* of a full-passthrough baseboard).

## 5. Discovery (SwiftGPUNode extension)

The GPU discovery DaemonSet already finds GPUs, NUMA, and **NVSwitches**
(`SwiftGPUNode.status.nvSwitches`) and the host Fabric Manager. Tier 3 needs the
**baseboard grouping + PCIe hierarchy**:

- **Baseboard membership** — which GPUs + NVSwitches form one HGX baseboard (so
  allocation knows the whole-baseboard set). Derive from the NVLink/NVSwitch
  topology (`nvidia-smi topo`, the FM topology files) + PCIe ancestry.
- **PCIe hierarchy** — the switch tree (`lspci -t`, sysfs ancestry) needed to
  synthesize strategy (A), recorded per baseboard.
- **IOMMU groups** — the full set of devices (GPUs, NVSwitches, switch ports,
  companions) that must bind to `vfio-pci` together, extending the existing
  IOMMU-group peer-bind logic (the Tier 1 HD-Audio companion case, generalized to
  a whole baseboard).
- A `status` representation of "baseboard N: GPUs[...], NVSwitches[...], free/allocated".

## 6. Allocation (whole-baseboard)

Tier 3 allocation is **coarse-grained**: a guest gets an entire baseboard or
nothing. The SwiftGPU controller's `findAndAllocate` gains a **baseboard mode** —
match `count`/model against a *whole free baseboard*, mark all its GPUs +
NVSwitches `AllocatedTo` the guest, and pin the guest to that node. Release frees
the whole baseboard. This reuses the Phase 4 `ReserveOnNode`/`ReleaseFromNode`
primitives (TFU #27) — at baseboard granularity.

## 7. Runtime — the QEMU full-hierarchy builder

In `swift-qemu-client` (the QEMU path already used for Tier 2), Tier 3 generates:

- The **PCIe hierarchy** (strategy A): `-device pcie-root-port,...` +
  switch/bridge elements mirroring the discovered topology.
- **All GPUs + NVSwitches** as `vfio-pci` at the mirrored positions; large-BAR
  GPUs (B200) get `x-no-mmap=true` (`noMmap`).
- **NUMA + vCPU pinning** — an 8-GPU HGX is dual-socket; the existing
  `numaTopology`/`vcpuPinning` builder pins vCPUs to the sockets the GPUs hang
  off and builds matching guest NUMA nodes + 1 GiB hugepages.
- OVMF firmware (the QEMU path), 1 GiB hugepages (required for GPU workloads).

**CH-on-the-horizon note:** CH v52's `host_mmap_bars` (the native equivalent of
QEMU `x-no-mmap`), `iommufd`/`vfio-cdev`, sub-page BAR expansion, and lazy GSI
(many-device guests) could *eventually* let some large-BAR/HGX topologies run on
**Cloud Hypervisor** instead of QEMU — shrinking the QEMU dependency. Out of
scope for the first Tier 3 (QEMU is the validated multi-device path), but
evaluate during the spike now that the v52 knobs exist.

## 8. Fabric Manager in the guest

- The **guest image** ships the `nvidia-open` driver + **Fabric Manager**, with
  the FM version **exactly matching** the driver (the same host-FM-version
  discipline, moved into the guest). `fabricManager.requiredVersion` is the gate
  the controller validates.
- On boot, the **guest FM** initializes the NVSwitch fabric it now owns. There is
  **no host FM** for this baseboard.
- `gpu-init` (the init container) binds the **whole baseboard** (all GPUs +
  NVSwitches + switch ports + companions) to `vfio-pci` using the two-pass
  unbind-all-then-bind-all procedure (the PR #93 fix), and does **NOT** activate a
  host FM partition (Tier 3 has none).

## 9. Interactions / non-goals (v1 Tier 3)

- **Live migration** of a full-baseboard GPU guest — never (VFIO can't
  live-migrate; offline release-and-reallocate per TFU #27 applies, at baseboard
  granularity).
- **Snapshots** of a Tier 3 guest — VFIO state can't be CH-restored; not a target.
- **Confidential + HGX** — confidential GPU is a separate future track
  (see [`swiftconfidential-sev-snp.md`](swiftconfidential-sev-snp.md) §9).
- **Partial baseboard** — that's Tier 2 (`hgx-shared`); Tier 3 is all-or-nothing.

## 10. Open questions / spike plan (on real HGX hardware)

1. Does the **synthesized PCIe hierarchy (A)** satisfy CUDA + in-guest Fabric
   Manager, or is **switch passthrough (B)** required?
2. The exact **baseboard IOMMU-group** set + ACS/peer constraints for whole-
   baseboard VFIO bind.
3. **Guest FM init** ordering vs the driver, and the driver/FM version matrix per
   HGX generation (H100/H200/B200).
4. Can any of it move to **CH v52** (`host_mmap_bars` + `iommufd`), or is QEMU
   still required for the multi-device PCIe hierarchy?
5. NUMA/pinning + hugepage sizing for the dual-socket 8-GPU case.

Until then: **design-complete, asset-gated** — like Tier 2 GPU and multi-NIC.

## 11. Phased PR breakdown (provisional — after the spike)

| PR | Scope |
|---|---|
| 0 | Spike on real HGX: PCIe-hierarchy strategy (A vs B), in-guest FM init, IOMMU-group set. |
| 1 | This design doc. |
| 2 | Discovery: baseboard grouping + PCIe hierarchy + full IOMMU-group set on `SwiftGPUNode`. |
| 3 | Webhook tightening (`hgx-full` ⇒ `partitionMode: full` + `runInGuest`; `count` = baseboard size); whole-baseboard allocation (reusing the Phase 4 reserve/release primitives). |
| 4 | Runtime: the `swift-qemu-client` full-hierarchy builder (synthesized PCIe tree + all-device VFIO + NUMA/pinning/hugepages). |
| 5 | `gpu-init` whole-baseboard bind; in-guest FM image guidance + the driver/FM version gate; operator runbook. |
| (future) | Evaluate CH v52 (`host_mmap_bars`/`iommufd`) to reduce the QEMU dependency. |

🤖 Generated with [Claude Code](https://claude.com/claude-code)
