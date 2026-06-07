# Windows Guest Support — Boot Spike Findings

> Status: SPIKE COMPLETE (2026-06-07). Answers the load-bearing unknowns from
> [`windows-guest-support.md`](windows-guest-support.md) §6 — **before** committing
> to the phased build. Headline: the image-prep pipeline works and **QEMU+OVMF
> boots Windows cleanly and stably**, but **Cloud Hypervisor v51.1 is BLOCKED** by a
> `viostor.sys` `0xD1` bugcheck on its virtio-blk device. This **flips the design's
> CH-first OQ1 resolution** — see §7.

## 1. Goal & method

Validate, end to end and entirely off-cluster (the dev cluster has no Windows
image/license), whether a virtio-ready Windows Server guest boots on **Cloud
Hypervisor** via the existing `--kernel CLOUDHV.fd` disk-boot path, and whether
the **QEMU+OVMF escape hatch** boots it. Everything ran locally with the **real
CH v51.1 binary + `CLOUDHV.fd`** extracted from the `swiftletd` image, so the CH
result reflects exactly what KubeSwift ships.

## 2. Environment

| Component | Version / detail |
|---|---|
| Cloud Hypervisor | **v51.1** (binary + `CLOUDHV.fd` extracted from `swiftletd:sha-5c3bc95`) |
| QEMU (install + escape-hatch) | `/usr/bin/qemu-system-x86_64` 8.2.2 (Debian) — VNC + slirp + std-VGA |
| QEMU (kata-static, rejected) | `/opt/kata/bin/...` 9.1.2 — no VNC, no slirp, no VGA romfiles (see §6 W-S1) |
| Guest OS | Windows Server 2022 Eval (build 20348), `install.wim` index 1 = Standard Core |
| virtio drivers | `virtio-win.iso` (stable, 0.1.x) — viostor / NetKVM injected at install |
| Firmware | QEMU: `OVMF_CODE_4M.fd`; CH: `CLOUDHV.fd` (EDK2 UEFI) |

Disk: 40 GiB GPT, ESP(260M)/MSR(128M)/NTFS. Raw conversion for CH (`qemu-img
convert -O raw`), ~5.4 GiB sparse.

## 3. What works ✅

1. **Unattended virtio install (QEMU/KVM)** — `autounattend.xml` does a UEFI/GPT
   install **onto a virtio-blk disk** with **viostor injected** during WinPE
   (`DriverPaths` → `\viostor\2k22\amd64`). Reaches OOBE → auto-logon → first-logon
   commands → clean self-shutdown. **Repeatable in ~3.5 min.** Proves the
   driver-injection image-prep pipeline.
2. **Fallback-path boot** — Windows Setup writes `bootmgfw.efi` to **both**
   `\EFI\Microsoft\Boot\` *and* the firmware fallback `\EFI\Boot\bootx64.efi`
   (byte-identical), with the BCD at `\EFI\Microsoft\Boot\BCD`. CH (and any
   empty-NVRAM firmware) boots via the fallback path — **no NVRAM seeding needed**.
3. **Headless BCD prep is required and sufficient (for QEMU)** — set in
   first-logon: `bcdedit /ems {current} on`, `/bootems {bootmgr} on`,
   `/emssettings EMSPORT:1 EMSBAUDRATE:115200`, **`recoveryenabled no`**,
   **`bootstatuspolicy ignoreallfailures`**. Without it, a fallback-path boot drops
   into **graphical "Preparing Automatic Repair" (WinRE)** that hangs on a
   console-less VMM. With it, the image **boots cleanly under QEMU+OVMF to SAC**
   (serial: `Computer is booting, SAC started and initialized … SAC>`), no crash,
   stable. **The QEMU+OVMF escape hatch is validated.**

## 4. What is BLOCKED ❌ — Cloud Hypervisor v51.1

CH loads the Windows Boot Manager and the kernel runs, but Windows **bugchecks in
the virtio-blk driver** and reboot-loops. Established step by step:

1. **CH firmware → kernel handoff happens.** `CLOUDHV.fd` reaches the OS loader via
   EDK2 **PlatformRecovery** (`\EFI\Boot\bootx64.efi`), then `VirtioRngExitBoot` +
   `MpInitChangeApLoopCallback() done!` — i.e. `ExitBootServices` fired and
   **winload handed off to the NT kernel**. `\Windows\bootstat.dat` is rewritten on
   CH (serial-independent proof the kernel ran).
2. **`kvm_hyperv=on` is REQUIRED.** Without `--cpus …,kvm_hyperv=on` the kernel
   **silently hangs in early MP/HAL init** — serial frozen at the firmware
   baseline, no SAC, no `bootstat` update. With it, the kernel boots and **SAC
   initializes** (`SAC>`, "The CMD command is now available").
3. **Then: `0xD1` in `viostor.sys`.** Shortly after SAC, Windows bugchecks. Pulled
   directly from the CH-written crash dump:
   ```
   BugCheckCode : 0x000000D1   (DRIVER_IRQL_NOT_LESS_OR_EQUAL)
   Param2       : 0x0A         (IRQL = DISPATCH_LEVEL)
   faulting drv : \Driver\viostor  (dump_viostor, viostor service)
   ```
   `C:\Windows\Minidump\*.dmp` + a 1.4 GB `MEMORY.DMP` are written on the CH disk;
   `AutoReboot` then warm-resets → **reboot loop**. (CH v51.1 also **exits on guest
   warm-reset** rather than resetting in place, so each launch shows the same
   ~2.7 KB SAC serial + 2 resets + exit.)
4. **Not a config/SMP issue.** The `0xD1` reproduces with **`num_queues=1`** and
   with **a single vCPU (`boot=1`)** — ruling out multi-queue and SMP-race
   explanations. It is a **fundamental incompatibility between the (stable) viostor
   driver and CH v51.1's virtio-blk device** (feature-negotiation / virtqueue
   handling), not something a launch flag fixes.

## 5. Findings table

| # | Finding | Evidence |
|---|---|---|
| F1 | Virtio (viostor) Windows install automatable & fast | install SUCCESS, qcow2 5.5 GB, COM1 sentinel |
| F2 | Setup creates the `\EFI\Boot\bootx64.efi` fallback (== bootmgfw); BCD present | md5 match; ESP listing |
| F3 | Headless BCD prep (EMS + disable Auto-Repair) is mandatory for any console-less VMM | empty-NVRAM QEMU repro: pre-prep → "Automatic Repair"; post-prep → SAC |
| F4 | **QEMU+OVMF boots Windows cleanly & stably** | SAC up, no crash, no loop |
| F5 | CH needs `kvm_hyperv=on` or the kernel hangs pre-SAC | with/without comparison |
| F6 | **CH v51.1 bugchecks `0xD1` in viostor → reboot loop** | `MEMORY.DMP` bugcheck `0xD1`, `\Driver\viostor` |
| F7 | F6 is not SMP/queue-count related | reproduced at `num_queues=1` and `boot=1` |
| F8 | CH v51.1 exits on guest warm-reset (no reset-in-place) | `DXE ResetSystem2: ResetType Warm` then CH exits |

## 6. Operational notes (for re-running the spike)

- **W-S1 — use the full QEMU, not kata-static.** `/opt/kata/bin/qemu` (9.1.2) has
  no VNC, no `user` netdev (slirp), and no VGA romfile (`vgabios-stdvga.bin`) — it
  rejects `-vnc`/`-netdev user` and dies on the default VGA. The Debian
  `/usr/bin/qemu` (8.2.2) has all three; its **VGA framebuffer is screendump-able
  over QMP**, which is what made Setup's silent failures diagnosable.
- **Headless install gotchas:** the UEFI "Press any key to boot from CD" prompt
  needs a QMP `send-key` spammer (~30 s); the answer file needs
  `Microsoft-Windows-International-Core-WinPE` to auto-skip the language screen; and
  **eval media is keyless** — including a `<ProductKey>` (even empty) throws
  "cannot find the Microsoft Software License Terms".
- **Success signal without RDP/VNC:** a first-logon `echo …>COM1` sentinel + a
  `shutdown /s` (detect clean QEMU exit). The slirp `hostfwd` RDP probe gives a
  **false positive** (the host-side listener accepts immediately).
- **CH boot signal:** `bootstat.dat` mtime (winload rewrites it every boot) and
  SAC/EMS on `--serial file=…` are the serial-independent / serial proofs that the
  kernel ran.

## 7. Design implications — OQ1 must be revisited

The design doc resolved **OQ1 (hypervisor) → Cloud Hypervisor default** on the
premise "CH supports Windows." **This spike does not validate that on CH v51.1**:
the virtio-blk (`viostor`) `0xD1` bugcheck is a hard blocker, and CH's headless,
exit-on-reset, no-VGA model also removes the ability to run a graphical
install/recovery. By contrast **QEMU+OVMF — the design's "escape hatch" — boots
Windows cleanly and stably today.**

Recommendation for the kickoff conversation (the user's call, since this flips a
decision they previously steered to CH-first):

- **v1 Windows = QEMU+OVMF** (the escape hatch becomes the primary, and for now the
  only, working path), selected by the `osType: windows` gate — mirrors "QEMU only
  when the hardware/feature requires it" (here: the guest OS requires it). Most of
  the design (osType gate, import-step skipping, cloudbase-init over NoCloud, the
  BCD/virtio image-prep runbook) is **unchanged** — only the hypervisor default
  flips for `osType: windows`.
- **CH-for-Windows is a future track**, gated on clearing F6. Untested mitigations,
  in priority order: (a) a **newer `virtio-win` viostor** (the stable build here may
  predate CH virtio-blk fixes); (b) a **newer Cloud Hypervisor**; (c) upstream CH
  virtio-blk/viostor interop work. If (a) or (b) clears F6, CH-first can be
  reconsidered. The spike's image-prep + `kvm_hyperv=on` + fallback-path findings
  all carry forward.

## 8. Reproduction artifacts

Local, off-repo (`/home/wrkode/win-spike/`, not committed — large ISOs/images):
`autounattend.xml` (virtio + headless BCD prep), `run-install.sh` (QMP key-spammer,
COM1 sentinel, exit-detect), `win.qcow2`/`win.raw` (the virtio-ready WS2022 image),
and the CH crash dump (`MEMORY.DMP` bugcheck `0xD1`/`viostor`). The CH boot command
that reaches SAC-then-crash:

```
cloud-hypervisor --kernel CLOUDHV.fd \
  --disk path=win.raw,num_queues=1 \
  --cpus boot=2,kvm_hyperv=on \
  --memory size=4096M --serial file=ch-serial.log --console off
```

🤖 Generated with [Claude Code](https://claude.com/claude-code)
