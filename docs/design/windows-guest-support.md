# Windows Guest Support

> Status: DESIGN — **spike complete, OQ1 RESOLVED → CH-first (on CH v52.0)**. First
> scoping pass for running Windows VMs as SwiftGuests. Greenfield — there is no
> `osType` concept in the codebase today; several runtime layers assume a Linux
> guest. Last updated: 2026-06-07.
>
> **Spike result (see [`windows-guest-support-spike.md`](windows-guest-support-spike.md)):**
> the image-prep pipeline works, QEMU+OVMF boots Windows cleanly, and — the decisive
> follow-up — **Cloud Hypervisor v52.0 boots Windows cleanly and stably**. The
> `viostor.sys` `0xD1` crash that blocked the first pass was a **CH _v51.1_
> virtio-blk bug, fixed in v52.0** (verified: stable >180 s, `bootstat`+`evtx`
> updated, zero crash dumps, default queues). So **`osType: windows` stays on Cloud
> Hypervisor**, conditioned on **bumping the shipped CH v51.1 → v52.0** (a
> platform-wide change — needs Linux-guest regression validation). Windows needs
> `--cpus kvm_hyperv=on` + a virtio-ready image + headless BCD prep. **QEMU+OVMF
> reverts to the escape hatch.** The rest of the design (`osType` gate, import-step
> skipping, cloudbase-init, virtio+BCD image-prep runbook) stands.

## 1. Goal

Run **Windows guests** (Windows Server 2019/2022/2025, and Windows 10/11) as
first-class SwiftGuests — booted from a disk image, networked, and provisioned,
the same way Linux guests are. Out of scope for v1: GPU passthrough to Windows,
live migration of Windows guests, snapshots of Windows guests (they should work
mechanically but are not the v1 validation target).

## 2. Cloud Hypervisor runs Windows — this is mostly "gate the Linux-only steps"

**Windows stays on Cloud Hypervisor** (the project default; QEMU only when a
feature genuinely requires it). CH is a documented, supported Windows VMM
(Server 2019/2022, Win10/11), and a Windows guest reuses **the same CH disk-boot
path KubeSwift already has** — `--kernel CLOUDHV.fd` (EDK2 UEFI) + virtio-blk
disk + tap/virtio-net. Most "Windows work" is *not* booting it; it's skipping the
Linux-only steps and preparing the guest.

| Layer | Linux today | Windows |
|---|---|---|
| **Hypervisor** | Cloud Hypervisor (CLOUDHV.fd via `--kernel`) | **Same — Cloud Hypervisor, on CH v52.0** (spike-confirmed). CH v51.1 bugchecked `0xD1` in `viostor.sys`, but **CH v52.0 boots Windows cleanly and stably** ([spike](windows-guest-support-spike.md) §4.1) — reuse the existing disk-boot path with `--cpus kvm_hyperv=on`. Requires bumping the shipped CH v51.1 → v52.0 (platform-wide). QEMU+OVMF stays the escape hatch. |
| **Firmware** | CLOUDHV.fd (EDK2 UEFI) for disk boot | **Same CLOUDHV.fd.** Windows is UEFI-only; the existing EDK2 UEFI firmware path already provides it. Not a divergence. |
| **virtio drivers** | in-tree (Linux ships virtio-blk/net) | The real Windows problem — Windows has **no** virtio drivers OOTB, so a stock image sees neither the virtio-blk disk nor the virtio-net NIC. **This is hypervisor-agnostic** (identical for CH and QEMU; both present virtio devices). §3. |
| **Provisioning** | cloud-init NoCloud seed.iso | **cloudbase-init** reads the same NoCloud/ConfigDrive seed the runtime already builds. Hypervisor-agnostic. |
| **Image import** | qcow2→raw + **GRUB serial patch** + cloud-init `growpart` | Skip `patch_grub` and growpart (no GRUB, no cloud-init growpart on Windows); keep qcow2→raw + `qemu-img resize`. Disk extend is Windows-side (diskpart/unattend). Hypervisor-agnostic. |
| **Console** | serial socket (`swiftctl console` over `--serial`) | **The one genuine CH gap.** CH is serial/headless — no VNC. A pre-prepared (virtio-ready) image runs headless on CH and is managed over **RDP/WinRM**; an in-cluster *graphical* install or troubleshooting a network-broken guest is where the QEMU+VNC escape hatch earns its place. |

The throughline: **CH already does Windows**; ~5 of the 6 layers are
hypervisor-agnostic. The only thing CH can't give Windows is a graphical
console, and that's only needed for graphical install / driver injection /
no-network troubleshooting — exactly the narrow case the QEMU escape hatch
covers.

## 3. The central problem: virtio drivers (hypervisor-agnostic)

A stock Windows ISO/image cannot see virtio-blk or virtio-net — true on CH
**and** QEMU, since both present virtio devices. Three ways out (none of which
is a reason to abandon CH):

- **(A) Operator brings a virtio-ready image** — the operator prepares a Windows
  disk image with the `virtio-win` drivers already installed (the standard
  KubeVirt/OpenStack practice), imports it as a SwiftImage, and **CH boots it
  with virtio-blk/net, headless, managed over RDP.** Minimal KubeSwift work;
  image prep is a documented runbook. **Recommended for v1.**
- **(B) Emulated devices (no virtio)** — boot the disk on SATA/AHCI and the NIC
  on e1000, which Windows drives OOTB. Works with a stock image but is slower
  and **requires QEMU** (CH is virtio-only). A fallback for stock images.
- **(C) Unattended driver injection** — attach the `virtio-win` ISO + an
  `autounattend.xml` that installs drivers during Windows Setup. Most automated,
  most moving parts; needs a graphical/console-capable run (QEMU+VNC) for the
  install. A later phase.

Recommendation: **v1 = (A)** on Cloud Hypervisor (virtio-ready image,
documented prep). (B)/(C) ride the QEMU escape hatch when an operator can't
pre-prepare an image.

## 4. Proposed shape

1. **`osType` on SwiftGuest (and SwiftImage)** — `linux` (default) | `windows`.
   It gates the Linux-only import steps, the provisioning datasource, the
   resize expectation, and (only when escape-hatch features are requested) the
   hypervisor. The single decision point — mirrors how `gpuProfileRef.tier`
   drives CH-vs-QEMU today.
2. **Hypervisor — Cloud Hypervisor by default.** `osType: windows` runs on the
   existing CH disk-boot path (CLOUDHV.fd + virtio). **QEMU + OVMF is the opt-in
   escape hatch**, selected only when the operator requests a graphical/VNC
   console or an emulated device model (stock non-virtio image). The selector is
   explicit (e.g. a `windows.console: serial|vnc` and/or `deviceModel:
   virtio|emulated` — `vnc`/`emulated` ⇒ QEMU), exactly like the GPU tier picks
   the runtime. Default Windows is CH-first.
3. **Image import** — skip `patch_grub` and the growpart expectation for
   `osType: windows`; still qcow2→raw + `qemu-img resize`. An `osType` on
   SwiftImage branches the import Job.
4. **Provisioning** — cloudbase-init reading the **existing NoCloud seed** (least
   new mechanism; the runtime already builds seed.iso). autounattend.xml is a
   follow-on for path (C).
5. **Console** — v1 on CH is **headless** (manage via RDP/WinRM); `swiftctl
   console` (serial) is best-effort. VNC arrives with the QEMU escape hatch.
6. **Networking** — same tap0/br0 model; virtio-net (image A) or e1000 (B).

## 5. Open decisions (for the kickoff conversation)

- **OQ1 — Hypervisor: RESOLVED → Cloud Hypervisor (on CH v52.0).** The first spike
  pass hit a `0xD1 viostor` bugcheck on **CH v51.1**; the CH-version follow-up showed
  **CH v52.0 boots Windows cleanly and stably** — the `0xD1` was a v51.1 virtio-blk
  bug, fixed in v52.0 ([spike](windows-guest-support-spike.md) §4.1). So
  `osType: windows` **stays on Cloud Hypervisor** (principle-consistent),
  **conditioned on bumping the shipped CH v51.1 → v52.0** (platform-wide — needs
  Linux-guest regression validation). Required: `--cpus kvm_hyperv=on`. **QEMU+OVMF
  is the escape hatch** for graphical-install / emulated-device cases and the interim
  fallback if the CH bump is deferred.
- **OQ2 — virtio strategy for v1:** (A) operator-prepped virtio image on CH
  (recommended) vs (B) emulated devices on the QEMU escape hatch for stock
  images.
- **OQ3 — provisioning:** cloudbase-init over the existing NoCloud seed
  (recommended) vs an autounattend.xml datasource (new SwiftSeedProfile type).
- **OQ4 — console for v1:** headless-on-CH + RDP (recommended; principle-first)
  vs building the QEMU+VNC escape hatch in v1 for graphical install/troubleshoot.
- **OQ5 — validation:** the dev cluster has **no Windows image/license**.
  Windows Server eval ISOs (180-day, no key) are obtainable but large, and a
  virtio-ready image must be prepared off-cluster first. Is cluster validation in
  scope now, or do we ship behind the same "asset not available" caveat as Tier
  2/3 GPU until a Windows image exists?

## 6. Spike — COMPLETE (2026-06-07)

Full findings: [`windows-guest-support-spike.md`](windows-guest-support-spike.md).
Ran entirely off-cluster with the **real CH v51.1 binary + `CLOUDHV.fd`** from the
`swiftletd` image. Answers to the load-bearing unknowns:

1. **Does a virtio-ready Windows guest boot on Cloud Hypervisor?** — **NO on CH
   v51.1, YES on CH v52.0.** On v51.1 the kernel runs (needs `--cpus kvm_hyperv=on`)
   and SAC initializes, then Windows bugchecks **`0xD1` in `viostor.sys`** and
   reboot-loops. The CH-version follow-up showed this is a **v51.1 virtio-blk bug
   fixed in v52.0**: the same image under **CH v52.0** boots **cleanly and stably**
   (>180 s, no reset, zero crash dumps, `bootstat`+`evtx` updated; default queues).
2. **Does QEMU+OVMF boot it?** — **YES, cleanly and stably** (SAC up, no crash). The
   image-prep pipeline (unattended virtio install + headless **BCD prep**: EMS/SAC +
   `recoveryenabled no` + `bootstatuspolicy ignoreallfailures`) is validated here.
3. **cloudbase-init / NoCloud seed** — **not exercised** (gated behind a booting
   guest; the install used `autounattend.xml`). Carried forward to the QEMU-path
   build (PR 5).

Net: the spike **confirms OQ1 → CH-first on CH v52.0**. The runtime PR routes
`osType: windows` to the existing CH disk-boot path with `--cpus kvm_hyperv=on`,
**gated on bumping the shipped CH v51.1 → v52.0** (platform-wide; run the
Linux-guest regression pass with it). QEMU+OVMF stays the escape hatch (and the
interim fallback if the bump is deferred). The image-prep, `kvm_hyperv=on`, and
`\EFI\Boot\bootx64.efi` fallback-path findings carry forward to whichever VMM.

## 7. Phased PR breakdown (refined after the spike — CH-first on CH v52.0)

The spike confirmed CH-first **on CH v52.0** (§6/OQ1). The runtime PR reuses the
existing CH disk-boot path; a **prerequisite** is the CH v51.1 → v52.0 bump (its
own change, with the Linux-guest regression pass). The `osType` gate, import-skip,
provisioning, and image-prep PRs are hypervisor-independent.

| PR | Scope |
|---|---|
| 0 | **Prereq: bump CH v51.1 → v52.0** in the `swiftletd` image (+ matching `CLOUDHV.fd`) with a Linux-guest regression pass. Platform-wide; gates the Windows runtime path. |
| 1 | This design doc (+ spike findings doc). |
| 2 | `osType` field on SwiftGuest + SwiftImage (+ webhook rules) + resolver wiring. Default `linux` — no behavior change for existing guests. |
| 3 | Image import: skip GRUB/serial patch + growpart for `osType: windows` (keep qcow2→raw + resize). |
| 4 | Runtime: `osType: windows` boots on the **existing CH disk-boot path** (CLOUDHV.fd + virtio) via the `\EFI\Boot\bootx64.efi` fallback, adding `--cpus kvm_hyperv=on` and gating off Linux-only cmdline/console assumptions. (Mostly reuse; QEMU+OVMF remains the escape hatch / interim fallback.) |
| 5 | Provisioning: cloudbase-init userdata over the NoCloud seed (the spike used `autounattend.xml`; cloudbase-init is the runtime path). |
| 6 | Image-prep tooling/runbook: virtio (viostor/NetKVM) + the **headless BCD prep** (EMS/SAC, `recoveryenabled no`, `bootstatuspolicy ignoreallfailures`) the spike validated; the `autounattend.xml` + `run-install.sh` from the spike are the seed. |
| 7 | Operator runbook + samples; cluster validation (asset-gated — no Windows license on the dev cluster). |

## 8. Non-goals (v1)

GPU passthrough to Windows; Windows live migration; Windows snapshots as a
validation target; autounattend.xml driver injection (path C) as the default;
VNC as the default console (it's the escape-hatch path).

## 9. Risks

- **virtio dependency** is the make-or-break — a stock image won't boot on
  virtio; v1 leans on operator image prep (documented) or the QEMU emulated
  escape hatch.
- **Console on CH** — Windows on CH is headless (manage via RDP/WinRM); the
  graphical console for install/troubleshooting is the QEMU+VNC escape hatch.
  v1 assumes a pre-prepared image so the headless path suffices.
- **Validation asset gap** — no Windows image on the dev cluster; this may force
  an asset-gated ship.
- **Scope creep** — Windows touches several layers; the `osType` gate must stay
  the single decision point (mirror the GPU-tier pattern) to avoid Linux-path
  regressions.

🤖 Generated with [Claude Code](https://claude.com/claude-code)
