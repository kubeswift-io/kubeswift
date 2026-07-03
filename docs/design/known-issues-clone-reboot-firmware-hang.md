# Known issue — cloneFromSnapshot guests fail to reboot (root-disk geometry, NOT a firmware hang)

> Status: **ROOT CAUSE IDENTIFIED (2026-07-03, cluster-reproduced with a
> deterministic signal). The "CH v52 firmware hang" title/framing was a
> MISDIAGNOSIS** — see "ROOT CAUSE" immediately below. The real fault is a
> KubeSwift **clone root-disk geometry bug** (`internal/controller/swiftguest/rootdisk.go`),
> not EDK2/Cloud Hypervisor. Severity: MEDIUM (a memory-snapshot clone cannot
> reboot; unaffected: normal guests + the source). Effectively **mitigated** for
> the primary need by the shipped in-guest vsock identity agent (v0.4.4), which
> regenerates identity + re-DHCPs with no reboot.

## ROOT CAUSE (2026-07-03, cluster-reproduced, deterministic)

A `cloneFromSnapshot` clone's root disk is **cloned from the pristine SwiftImage**
(`EnsureRootDiskClone` → `sourcePVC := rg.PreparedImage.PVCName`; `createCloneJob`
does `cp image.raw` + `qemu-img resize` + `sgdisk -e` — **no `growpart` /
`resize2fs`**). On a *normal* guest that is correct: cloud-init `growpart` grows
the partition + `resize2fs` grows the filesystem on **first boot**. But a clone
**RESUMES** captured RAM — **cloud-init never re-runs** — so the on-disk partition
stays at the pristine image size while the resumed guest's in-RAM ext4 is the
**source's already-grown** filesystem. The resumed guest syncs that grown
superblock onto the small-partition clone disk, leaving it internally
inconsistent: **filesystem size > partition size**.

It works while running (the mount lives in the captured RAM). It only breaks on
the **next reboot**, when the initramfs re-reads the disk and ext4 refuses:

```
EXT4-fs (vda1): bad geometry: block count 2359035 exceeds size of device (655099 blocks)
```

→ the guest drops to the **initramfs emergency shell**, never reaches systemd,
never re-DHCPs, and is unreachable — which the operator saw as a "hang".

**Cluster reproduction (2026-07-03, field-testing, boba, CH v52.0, deterministic
`DHCPACK`+`boot_id` signal):**

| Guest | root disk | partition `vda1` | ext4 superblock | reboot result |
|---|---|---|---|---|
| `rh-src` (normal source) | grown on first boot | 18,872,287 sec ≈ 9.66 GB | 2,359,035 blk ≈ 9.66 GB (**match**) | **rebooted cleanly in ~15 s** (new boot_id, 2×DHCPACK) |
| `rh-clone` (cloneFromSnapshot) | pristine image copy | 5,240,799 sec ≈ 2.68 GB | 2,359,035 blk ≈ 9.66 GB (**fs > part**) | **initramfs; no DHCP; unreachable 4.3+ min** |

The two corrections from the earlier off-cluster attempt hold and explain why it
looked like firmware: (1) `MpInitChangeApLoopCallback() done!` is the firmware's
*normal* hand-off point (the debug port goes quiet there on every boot as the
kernel takes the console) — it is **not** an AP-init hang; the guest is past
firmware, in the kernel's initramfs. (2) the serial console is an unreliable
verdict tool; the deterministic signal is the launcher `DHCPACK` (as the
original 2026-06-15 `ft-golden` vs `ft-clone-a` observation already used).

**Fix direction:** a memory-snapshot clone must get a root disk whose on-disk
geometry (and data) matches the resumed RAM — i.e. **clone the disk from the
SOURCE's actual root PVC** (grown partition+fs), not from the pristine SwiftImage.
(Equivalently: grow the clone disk's partition+fs to the source's geometry before
resume — but cloning the source's disk is the clean fix and also gives the clone
the source's real disk content, not the pristine image's.) Scope: the
`cloneFromSnapshot` branch of `EnsureRootDiskClone`. Everything below this line is
the **original (superseded) firmware-hang hypothesis**, kept for history.

---

## Symptom

A `SwiftGuest.spec.cloneFromSnapshot` guest (CH `--restore` of an
`includeMemory: true` snapshot) reaches `Running` by **resuming** the source's
captured RAM byte-for-byte. The documented way to give the clone its own
identity + a fresh IP is to reboot it once (the seed's `kubeswift.clone=true`
bootcmd regenerates machine-id / SSH host keys / hostname, and the guest
re-runs DHCP). On **Cloud Hypervisor v52 that reboot hangs in UEFI firmware** —
the guest never reaches the kernel, so it never re-DHCPs and never regenerates
identity. `status.network.primaryIP` stays empty.

## Isolation (cluster-observed, field-testing, 2026-06-15)

Driving `sudo reboot` over the serial console on two guest classes on the same
node (boba), both with **2 vCPUs** and **`core_scheduling: "Vm"`** (identical):

| Guest | Kind | Reboot result |
|---|---|---|
| `ft-golden` | normal disk-boot | `reboot: Restarting system` → `MpInitChangeApLoopCallback() done!` → **`DHCPACK(br0) 192.168.99.20`** — booted in ~36s ✓ |
| `ft-clone-a` | cloneFromSnapshot (`--restore`) | `DXE ResetSystem2: ResetType Warm` → … → `MpInitChangeApLoopCallback() done!` → **HANG** (serial log frozen 3–5+ min; never reaches the kernel) ✗ |

So the hang is **specific to restored guests**. The differentiators ruled OUT:
- **vCPU count** — both are 2.
- **`core_scheduling`** — both report `"Vm"` (a CH default here, not clone-only).
- **CH process death** — CH stays alive across the hang (`vm.info` responds;
  `boot_vcpus: 2`, guest config intact). It is the *guest firmware* that wedges,
  not the VMM.

The only remaining differentiator is the `--restore` path itself.

## Evidence pointing at the EDK2 S3-resume / AP-init path

The restored guest's reboot loads `S3Resume2Pei.efi` and freezes right after
`MpInitChangeApLoopCallback() done!` (the multiprocessor / application-processor
init callback). A normal guest emits the same line and immediately proceeds to
the kernel. Reading: on a warm reset the restored guest's firmware enters an
**S3 (suspend-to-RAM) resume** code path — plausibly because the snapshot froze
ACPI/firmware state that makes EDK2 believe it is resuming from S3 — and then
**hangs bringing the APs back up**. CLOUDHV.fd (rust-hypervisor-firmware / EDK2)
+ CH v52 + restored memory state is the suspect surface.

## Impact

- **Memory-snapshot clones cannot diverge guest-visible identity or obtain a
  discoverable IP via the documented reboot.** They are usable as **warm,
  read-mostly replicas that share the source's identity** (collision-safe because
  each clone is in its own pod network namespace), but not as independently
  addressable VMs through the reboot path.
- **Normal guests and the snapshot source are unaffected** — they reboot and
  re-DHCP cleanly (`ft-golden` proven above). CH v52 reset-in-place for
  non-restored guests was already validated during the v52 bump.
- Composes with the lease-poller fix (v0.4.3): the poller now stays alive for
  restore guests, so the IP **would** surface automatically the moment a clone
  re-DHCPs — it just never gets the chance while the reboot is wedged.

## Hypotheses (original; see "Off-cluster investigation" below for what the 2026-07-03 attempt found)

1. **Disable guest S3 / ACPI sleep so the warm reset takes the cold-boot path.**
   Investigate whether CH or the firmware can be told the guest has no S3 (e.g. a
   `_S3` ACPI removal, a CH platform/firmware flag, or a kernel `acpi_sleep=`
   / `no_console_suspend` cmdline on the source before snapshot). If the firmware
   stops entering `S3Resume2Pei`, the AP-init hang may not trigger.
2. **Single-vCPU repro.** The hang is in AP (secondary-CPU) init. Confirm whether
   a 1-vCPU clone reboots cleanly — if so, the AP bring-up on resume-then-reset is
   the precise fault and a mitigation can target it.
3. **CH version sweep.** Check CH `> v52` / upstream rust-hypervisor-firmware for a
   fixed restore-then-reboot path; this is a strong **upstream-CH candidate**.
4. **Sidestep the reboot entirely** — the planned **in-guest vsock identity
   agent** regenerates machine-id / SSH keys / hostname and renews DHCP *without*
   a reboot. That is the real fix for clone identity/IP and makes the reboot path
   unnecessary; prioritize it over chasing the firmware hang.

## Off-cluster investigation (2026-07-03) — inconclusive; two corrections

An off-cluster reproduction harness was built with the **real CH v52.0 binary +
`CLOUDHV.fd`** (extracted from the `swiftletd` image) and a Noble guest, driving
`boot → snapshot → --restore (resume) → reboot` directly. Findings:

- **Restore + resume is rock-solid.** Every `--restore` (eager and `ondemand`/
  userfaultfd) reached `state=Running`; the vsock-agent-survives-restore property
  (PR-0 spike) holds.
- **A deterministic hang could NOT be reproduced.** Restored clones were driven
  through `sudo reboot` and CH `vm.reboot` dozens of times across
  eager/`ondemand` restore modes, with and without a virtio-net device. Some
  trials clearly rebooted to multi-user (e.g. `vm.reboot` on a no-net clone
  produced a full boot — 19 `Reached target` lines on the guest serial); others
  looked wedged. **The same configuration produced both "rebooted" and "hung"
  verdicts on different runs**, so the result is measurement-limited, not a clean
  repro. Root cause off-cluster is **unconfirmed**.
- **Methodology caveat (why the verdicts are noisy):** observing reboot outcome
  over a `--serial socket=` connection is unreliable — the guest resets the
  serial device on warm reset, so a held connection goes silent (looks hung) and
  intermittent fresh probes race the boot. A trustworthy repro needs a
  **connection-independent liveness signal** — the **vsock agent ping** (responds
  only once the guest reaches multi-user) is the right instrument; the serial
  console is not.

**Correction to the diagnosis above:** the "froze after
`MpInitChangeApLoopCallback() done!`" evidence was over-read. In the off-cluster
harness that line is the firmware's **normal** hand-off point — the firmware
debug port goes quiet there on **every** boot (successful or not) because the
kernel takes over the console. So "last line is `MpInit...done`" is **not**, by
itself, evidence of an AP-init firmware hang; the true stall point (when the
hang does occur on the cluster) is at or after the firmware→kernel hand-off and
remains unconfirmed. The "Evidence pointing at the EDK2 S3-resume / AP-init path"
section above should be read as a *hypothesis*, not a conclusion.

**Recommendation / next steps (revised):**
1. **Reproduce on the cluster** (where it was actually observed) using the
   **vsock agent ping** as the boot-completion signal — the only reliable way to
   separate "hung" from "slow/at-an-unresponsive-prompt".
2. **CH version bisect (v52 → v53+)** against a cluster repro; the intermittency
   is most consistent with a timing-sensitive upstream firmware/KVM interaction,
   not KubeSwift code — so this is the strongest lead for a real fix.
3. **Accept the in-guest vsock identity agent (shipped v0.4.4) as the resolution
   for the primary need.** It regenerates identity + renews DHCP *in place, no
   reboot*, so the reboot path is unnecessary for clone identity/IP. The
   reboot-hang then only affects *other* reboots (kernel updates, etc.) of a
   restored clone — a lower-severity, upstream-watch item.

The single-vCPU (Lead 2) and S3-disable (Lead 1) experiments were **not run to a
firm conclusion** — they depend on a reliable repro, which the off-cluster
harness could not provide.

## Related

- The lease-poller-stays-alive fix (v0.4.3): `rust/swiftletd/src/lease.rs`
  (`LEASE_POLL_ATTEMPTS_RESTORE`). Correct and necessary, but cannot deliver a
  clone IP while the reboot is wedged.
- Operator runbook caveat:
  [`docs/snapshots/clone-from-snapshot.md`](../snapshots/clone-from-snapshot.md)
  "Known limitation on Cloud Hypervisor v52".
- The resume-vs-boot identity-inheritance rule (Snapshot Phase 2) is the root
  reason a reboot is needed at all.
