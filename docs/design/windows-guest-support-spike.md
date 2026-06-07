# Windows Guest Support — Boot Spike Findings

> Status: SPIKE COMPLETE (2026-06-07), **+ CH-version follow-up**. Answers the
> load-bearing unknowns from [`windows-guest-support.md`](windows-guest-support.md)
> §6 — **before** committing to the phased build.
>
> **Headline (updated):** the image-prep pipeline works, **QEMU+OVMF boots Windows
> cleanly**, and — the decisive follow-up — **Cloud Hypervisor _v52.0_ also boots
> Windows cleanly and stably.** The `viostor.sys` `0xD1` crash that blocked the
> first pass was a **CH _v51.1_ virtio-blk bug, fixed in v52.0** (§4.1). So **CH-first
> stands** for `osType: windows`, conditioned on **bumping the shipped CH v51.1 →
> v52.0** (a platform-wide change — needs Linux-guest regression validation). The
> only non-default CH setting Windows needs is `--cpus kvm_hyperv=on`. See §7.

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

## 4. Cloud Hypervisor — BLOCKED on v51.1 ❌, WORKS on v52.0 ✅

### 4.0 On CH v51.1 (the shipped version): blocked

CH v51.1 loads the Windows Boot Manager and the kernel runs, but Windows
**bugchecks in the virtio-blk driver** and reboot-loops. Established step by step:

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
   explanations. It is a **virtio-blk-device-side bug in CH v51.1** (the viostor
   driver is unchanged across the v51.1/v52.0 runs — see §4.1).

### 4.1 On CH v52.0: WORKS — the `0xD1` is fixed ✅

Re-ran the **same virtio-ready image** under **CH v52.0** (latest release;
`cloud-hypervisor-static`, reusing the v51.1 `CLOUDHV.fd`), `--cpus kvm_hyperv=on`:

- **Stable boot, no crash.** CH stayed alive **>180 s** on the first boot with **no
  warm-reset and no BSOD** (v51.1 reset within ~60 s every cycle). SAC up
  (`SAC>`, "CMD command is now available").
- **Disk-verified real boot.** After the run: `bootstat.dat` rewritten (winload
  ran), `System.evtx`/`Application.evtx` updated (services started — a complete
  boot, not an early-SAC hang), and **zero crash dumps** (no `Minidump`, no
  `MEMORY.DMP`) — the `0xD1` did not occur.
- **No virtio-blk tuning needed.** v52.0 boots stably with **default queues** (the
  v51.1-era `num_queues=1` workaround is unnecessary). The **only** non-default CH
  setting Windows requires is `--cpus kvm_hyperv=on`.
- v52.0 also **resets in place** on a guest reboot (v51.1 exited on warm-reset, §F8).

Conclusion: the blocker was a **CH v51.1 virtio-blk bug fixed in v52.0**, not a
viostor/Windows defect. Windows-on-CH is viable on **v52.0**.

## 5. Findings table

| # | Finding | Evidence |
|---|---|---|
| F1 | Virtio (viostor) Windows install automatable & fast | install SUCCESS, qcow2 5.5 GB, COM1 sentinel |
| F2 | Setup creates the `\EFI\Boot\bootx64.efi` fallback (== bootmgfw); BCD present | md5 match; ESP listing |
| F3 | Headless BCD prep (EMS + disable Auto-Repair) is mandatory for any console-less VMM | empty-NVRAM QEMU repro: pre-prep → "Automatic Repair"; post-prep → SAC |
| F4 | **QEMU+OVMF boots Windows cleanly & stably** | SAC up, no crash, no loop |
| F5 | CH needs `kvm_hyperv=on` or the kernel hangs pre-SAC | with/without comparison |
| F6 | **CH v51.1 bugchecks `0xD1` in viostor → reboot loop** | `MEMORY.DMP` bugcheck `0xD1`, `\Driver\viostor` |
| F7 | F6 is not SMP/queue-count related (same viostor across CH versions) | reproduced at `num_queues=1` and `boot=1` |
| F8 | CH v51.1 exits on guest warm-reset (no reset-in-place) | `DXE ResetSystem2: ResetType Warm` then CH exits |
| **F9** | **CH v52.0 boots Windows cleanly & stably — the `0xD1` is FIXED** | alive >180 s, no reset/BSOD; `bootstat`+`evtx` updated, **0 crash dumps**; default queues OK; resets in place |

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

## 7. Design implications — OQ1 stays CH-first, gated on a CH v52.0 bump

The design doc resolved **OQ1 (hypervisor) → Cloud Hypervisor default** on the
premise "CH supports Windows." The first pass appeared to refute that (CH v51.1
`viostor` `0xD1`), but the **CH-version follow-up restores it**: **CH v52.0 boots
Windows cleanly and stably** (§4.1). So:

- **`osType: windows` stays on Cloud Hypervisor** — principle-consistent, reuses the
  existing `--kernel CLOUDHV.fd` disk-boot path. **Conditioned on bumping the shipped
  CH `v51.1 → v52.0`** in the `swiftletd` image. The bump is **platform-wide** (it
  changes the VMM for Linux guests too), so it must land with **Linux-guest
  regression validation** — treat it as its own change, not a Windows-only flag.
- **Required CH settings for Windows:** `--cpus …,kvm_hyperv=on` (Hyper-V
  enlightenments — mandatory; without it the kernel hangs pre-SAC). No virtio-blk
  queue tuning needed on v52.0. Plus a **virtio-ready image** + the **headless BCD
  prep** (§3.3), both hypervisor-agnostic.
- **QEMU+OVMF reverts to the escape hatch** (its original role) — for graphical
  install / driver-injection / emulated-device stock images, and as the fallback if
  the CH bump is deferred. It boots Windows today, so it is the safe interim if v52.0
  can't be adopted immediately.
- **Carry-forwards (hypervisor-independent):** the image-prep pipeline (virtio
  install + viostor injection), the `\EFI\Boot\bootx64.efi` fallback-path boot (no
  NVRAM seeding), and the headless BCD prep all hold on either VMM.

Open items for the build: confirm v52.0 with its **matching `CLOUDHV.fd`** (the
spike reused the v51.1 firmware), and run the v51.1→v52.0 Linux-guest regression
pass before shipping the bump.

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
