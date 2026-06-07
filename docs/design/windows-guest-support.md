# Windows Guest Support

> Status: DESIGN (pre-spike). First scoping pass for running Windows VMs as
> SwiftGuests. Greenfield — there is no `osType` concept in the codebase today;
> every layer assumes a Linux guest. Last updated: 2026-06-07.

## 1. Goal

Run **Windows guests** (Windows Server 2019/2022/2025, and Windows 10/11) as
first-class SwiftGuests — booted from a disk image, networked, and provisioned,
the same way Linux guests are. Out of scope for v1: GPU passthrough to Windows,
live migration of Windows guests, snapshots of Windows guests (they should work
mechanically but are not the v1 validation target).

## 2. Why this is not "just another image"

Six layers of the runtime assume a Linux guest. Each needs a Windows path:

| Layer | Linux today | Windows divergence |
|---|---|---|
| **Hypervisor** | Cloud Hypervisor (CLOUDHV.fd via `--kernel`) | CH is virtio-only and Linux-cloud-focused; Windows needs broad device support + a graphical console. **QEMU + OVMF** is the proven Windows path (KubeSwift already runs QEMU for GPU tiers). |
| **Firmware** | CLOUDHV.fd (EDK2 UEFI), or kernel boot | Windows is UEFI-only (modern) — needs OVMF (`OVMF_CODE.fd` + per-VM `OVMF_VARS.fd`), the QEMU GPU path's firmware. |
| **virtio drivers** | in-tree (Linux ships virtio-blk/net) | Windows has **no** virtio drivers OOTB → a stock image sees neither the `--disk` (virtio-blk) nor the NIC (virtio-net), so it can't even boot. This is the central problem. |
| **Provisioning** | cloud-init NoCloud seed.iso | Windows uses **cloudbase-init** (can read the same NoCloud/ConfigDrive seed) or an **autounattend.xml**. |
| **Image import** | qcow2→raw + **GRUB serial patch** + cloud-init `growpart` | Windows images have no GRUB and no growpart — the import's `patch_grub` and the first-boot partition-grow must be **skipped**; disk resize is Windows-side (diskpart/unattend). |
| **Console** | serial socket (`swiftctl console` over `--serial`) | Windows has no real serial console (SAC is minimal). Needs **VNC** (QEMU provides it; CH does not). |

The throughline: **Windows wants the QEMU runtime** (OVMF + emulated/virtio device choice + VNC), which KubeSwift already has for GPU. Windows is largely "reuse the QEMU path + gate the Linux-only steps."

## 3. The central problem: virtio drivers

A stock Windows ISO/image cannot see virtio-blk or virtio-net. Three ways out:

- **(A) Operator brings a virtio-ready image** — the operator prepares a Windows
  disk image with the `virtio-win` drivers already installed (the standard
  KubeVirt/OpenStack practice), imports it as a SwiftImage, and KubeSwift boots
  it with virtio-blk/net. **Minimal KubeSwift work**; pushes image prep to the
  operator (documented runbook). **Recommended for v1.**
- **(B) Emulated devices (no virtio)** — boot the disk on SATA/AHCI and the NIC
  on e1000, which Windows drives OOTB. Works with a stock image but is slower and
  **requires QEMU** (CH is virtio-only). A useful fallback / first-boot mode.
- **(C) Unattended driver injection** — attach the `virtio-win` ISO + an
  `autounattend.xml` that installs drivers during Windows Setup. Most automated
  but the most moving parts; a later phase.

Recommendation: **v1 = (A)** (virtio-ready image, documented prep), with **(B)**
as an explicit `deviceModel: emulated|virtio` escape hatch for booting a stock
image. (C) is a future enhancement.

## 4. Proposed shape

1. **`osType` on SwiftGuest (and SwiftImage)** — `linux` (default) | `windows`.
   It gates: hypervisor selection, firmware, the import Linux-only steps, the
   provisioning datasource, and the console mode. Mirrors how `gpuProfileRef.tier`
   already drives CH-vs-QEMU.
2. **Hypervisor** — `osType: windows` ⇒ **QEMU + OVMF** (reuse `swift-qemu-client`
   + the GPU path's OVMF handling). No CH-Windows path in v1.
3. **Image import** — skip `patch_grub` and the growpart expectation for
   `osType: windows`; still do qcow2→raw + `qemu-img resize` (Windows extends the
   partition via unattend/diskpart, not growpart). Likely an `osType` on
   SwiftImage so the import Job branches.
4. **Provisioning** — cloudbase-init reading the **existing NoCloud seed** (least
   new mechanism; the runtime already builds seed.iso). The seed `userData`
   becomes cloudbase-init userdata. autounattend.xml is a follow-on.
5. **Console** — QEMU VNC (a `-vnc` unix/tcp socket) surfaced for `swiftctl
   console` (or a new `swiftctl vnc`); serial stays best-effort.
6. **Networking** — same tap0/br0 model; virtio-net (image (A)) or e1000 (B).

## 5. Open decisions (for the kickoff conversation)

- **OQ1 — Hypervisor: QEMU-only for Windows (recommended), or also attempt CH?**
  CH-Windows is an unknown and CH lacks VNC; QEMU is proven. Lean QEMU-only.
- **OQ2 — virtio strategy for v1:** (A) operator-prepped virtio image
  (recommended) vs starting with (B) emulated devices for stock images.
- **OQ3 — provisioning:** cloudbase-init over the existing NoCloud seed
  (recommended) vs an autounattend.xml datasource (new SwiftSeedProfile
  datasource type).
- **OQ4 — console:** add VNC plumbing in v1, or ship headless-first (RDP/SSH into
  the guest) and add VNC later?
- **OQ5 — validation:** the dev cluster has **no Windows image/license**. Windows
  Server eval ISOs (180-day, no key) are obtainable but large, and a virtio-ready
  image must be prepared off-cluster first. Is cluster validation in scope now,
  or do we ship behind the same "hardware/asset not available" caveat as Tier 2/3
  GPU until a Windows image exists?

## 6. Spike (before committing to the full build)

Once OQ1/OQ2 are settled, a spike answers the load-bearing unknowns:

1. Does a virtio-ready Windows Server eval guest **boot** under
   `qemu-system-x86_64 -machine q35 -bios OVMF` with virtio-blk + virtio-net on
   the cluster (the swift-qemu-client launch path)?
2. Does cloudbase-init read the existing NoCloud seed.iso and apply hostname /
   admin password / network?
3. Does VNC over a unix socket give a usable console?

If the spike can't run (no Windows image obtainable on this cluster), the design
ships as "code-complete, hardware/asset-gated validation" — explicitly labelled,
like Tier 2/3 GPU and multi-NIC.

## 7. Phased PR breakdown (provisional — refined after the spike)

| PR | Scope |
|---|---|
| 1 | This design doc. |
| 2 | `osType` field on SwiftGuest + SwiftImage (+ webhook: `windows` ⇒ QEMU; reject kernelRef/gpuProfileRef combos as needed) + resolver wiring. |
| 3 | Image import: skip GRUB/serial patch + growpart for `osType: windows`. |
| 4 | Runtime: route `osType: windows` to the QEMU launcher with OVMF + device-model selection (virtio vs emulated). |
| 5 | Provisioning: cloudbase-init userdata over the NoCloud seed. |
| 6 | Console: QEMU VNC plumbing + `swiftctl`. |
| 7 | Operator runbook (virtio-ready image prep) + samples; spike/cluster validation (asset permitting). |

## 8. Non-goals (v1)

GPU passthrough to Windows; Windows live migration; Windows snapshots as a
validation target; autounattend.xml driver injection (path C); CH-Windows.

## 9. Risks

- **virtio dependency** is the make-or-break — a stock image won't boot on
  virtio; v1 leans on operator image prep (documented) or the emulated escape
  hatch.
- **Validation asset gap** — no Windows image on the dev cluster; this may force
  an asset-gated ship.
- **Console** — without VNC, Windows is effectively headless (manage via
  RDP/WinRM/SSH); VNC is the operability piece.
- **Scope creep** — Windows touches every layer; the `osType` gate must stay the
  single decision point (mirror the GPU-tier pattern) to avoid Linux-path
  regressions.

🤖 Generated with [Claude Code](https://claude.com/claude-code)
