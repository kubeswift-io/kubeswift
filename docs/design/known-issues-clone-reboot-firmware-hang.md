# Known issue — cloneFromSnapshot guests fail to reboot (root-disk geometry, NOT a firmware hang)

> Status: **RESOLVED (2026-07-03).** Root cause was cluster-reproduced with a
> deterministic signal (the "CH v52 firmware hang" title/framing was a
> **MISDIAGNOSIS** — see "ROOT CAUSE" below), and the fix
> (`maybeRootDiskFromSourceClone`, PR #323) is **cluster-validated**. The real
> fault was a KubeSwift **clone root-disk geometry bug**
> (`internal/controller/swiftguest/rootdisk.go`), not EDK2/Cloud Hypervisor.
> Severity was MEDIUM (a memory-snapshot clone could not reboot; unaffected:
> normal guests + the source). This PR supersedes the diagnosis-only PR #322.

## ROOT CAUSE (2026-07-03, cluster-reproduced, deterministic)

A `cloneFromSnapshot` clone's root disk was **cloned from the pristine SwiftImage**
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
| `rh-clone` (pre-fix clone) | pristine image copy | 5,240,799 sec ≈ 2.68 GB | 2,359,035 blk ≈ 9.66 GB (**fs > part**) | **initramfs; no DHCP; unreachable 4.3+ min** |

The two corrections from the earlier off-cluster attempt hold and explain why it
looked like firmware: (1) `MpInitChangeApLoopCallback() done!` is the firmware's
*normal* hand-off point (the debug port goes quiet there on every boot as the
kernel takes the console) — it is **not** an AP-init hang; the guest is past
firmware, in the kernel's initramfs. (2) the serial console is an unreliable
verdict tool; the deterministic signal is the launcher `DHCPACK` (as the
original 2026-06-15 `ft-golden` vs `ft-clone-a` observation already used).

## Fix (PR #323) — clone the root disk from the SOURCE's disk, not the pristine image

A new `maybeRootDiskFromSourceClone` (in
[`internal/controller/swiftguest/clone.go`](../../internal/controller/swiftguest/clone.go)),
called from `EnsureRootDiskClone` right after `maybeRootDiskFromOCI` and **before**
the pristine-image copy path. For a **memory-only** clone (Tier B local / Tier C
s3 without `includeDisk`) it materializes the clone's root PVC as a **CSI clone
(`spec.dataSource`) of the SOURCE guest's live root PVC** — matching size, class,
and volume mode. Because it is a byte copy of the source's disk, the on-disk
partition+filesystem geometry (**and the real data**) match the resumed RAM, so
the next reboot's initramfs finds a consistent disk. The clone PVC is
`RestoreSeeded`-labelled (so the image-copy path skips it) and `NeedsGrowInit`
is **false** (growing it would re-desync partition-vs-resumed-fs). Longhorn clones
an attached source volume without detaching it, so the running source is
undisturbed.

- **Source root PVC gone → fail loud.** A memory-only clone genuinely needs the
  source's disk; there is no correct silent fall-through. (The source-independent
  path is a **full-state `includeDisk` OCI snapshot**, which carries the grown
  disk in the artifact and is handled first by `maybeRootDiskFromOCI` — the
  source-clone path declines those.)
- **Secondary data bug also fixed.** Before this change a memory-only clone was
  actually running on the *pristine image's* disk content (masked by the resumed
  page cache until a reboot). It now carries the source's real disk.

**Cluster validation (2026-07-03, field-testing, boba, CH v52.0, controller
`sha-54fbd8d`):** source `rf-src` → memory snapshot `rf-snap` → clone `rf-clone`.
The clone's root PVC had `dataSource: {kind: PersistentVolumeClaim, name:
swiftguest-root-rf-src}` (a CSI clone of the source disk, **not** the pristine
image). Geometry post-materialize **and** post-reboot: partition 18,872,287 sec ==
fs 2,359,035 blk (both ≈9.66 GB, matching the source). Driving `sudo reboot`:

| Clone | root disk geometry | reboot verdict |
|---|---|---|
| `rh-clone` (pre-fix, pristine image) | fs 9.66 GB > partition 2.68 GB | initramfs "bad geometry"; no DHCP; unreachable 4.3+ min |
| **`rf-clone` (post-fix, source-cloned)** | partition == fs (both 9.66 GB) | **new boot_id `19a67802` (≠ resumed `fcf7f42e` — proof of a real reboot); `DHCPACK(br0) 192.168.99.17`; `login:` prompt; `status.network.primaryIP` populated** |

The reboot-to-regenerate-identity + get-a-fresh-IP path (the reason clones reboot
at all) now works — the clone re-DHCP'd to a fresh IP with a per-clone MAC.
Regression tests in
[`rootdisk_source_clone_test.go`](../../internal/controller/swiftguest/rootdisk_source_clone_test.go):
CSI-clones the source PVC (dataSource + RestoreSeeded + matching size/class, no
grow-init); source-gone fails loud; a full-state oci-disk snapshot is declined.

> The in-guest **vsock identity agent** (v0.4.4) remains the *preferred* way to
> give a clone its own identity + IP — it regenerates in place with **no reboot**,
> so it never touches this disk-geometry path. This fix makes the *reboot* path
> (kernel updates, operator reboots, the legacy `kubeswift.clone=true` bootcmd)
> correct as well.

---

Everything below this line is the **original (superseded) firmware-hang
hypothesis**, kept for history.

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

The only remaining differentiator is the `--restore` path itself. *(In hindsight:
the restored guest is past firmware and stuck in the initramfs on the
bad-geometry disk — see "ROOT CAUSE" above. The "specific to restored guests"
observation was right; the "firmware" attribution was wrong.)*

## Evidence pointing at the EDK2 S3-resume / AP-init path

The restored guest's reboot loads `S3Resume2Pei.efi` and freezes right after
`MpInitChangeApLoopCallback() done!` (the multiprocessor / application-processor
init callback). A normal guest emits the same line and immediately proceeds to
the kernel. Reading: on a warm reset the restored guest's firmware enters an
**S3 (suspend-to-RAM) resume** code path — plausibly because the snapshot froze
ACPI/firmware state that makes EDK2 believe it is resuming from S3 — and then
**hangs bringing the APs back up**. CLOUDHV.fd (rust-hypervisor-firmware / EDK2)
+ CH v52 + restored memory state is the suspect surface. *(Superseded: this line
is the normal firmware→kernel hand-off; the stall is later, in the initramfs.)*

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

## Hypotheses (original; superseded by the ROOT CAUSE above)

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
  repro. *(Explained by the ROOT CAUSE: the off-cluster Noble guest booted from a
  disk whose partition already matched its fs — no growpart delta — so it had no
  geometry inconsistency to trip over. The cluster clone tripped because its
  pristine-image disk had a small partition vs the source's grown fs.)*
- **Methodology caveat (why the verdicts are noisy):** observing reboot outcome
  over a `--serial socket=` connection is unreliable — the guest resets the
  serial device on warm reset, so a held connection goes silent (looks hung) and
  intermittent fresh probes race the boot. A trustworthy repro needs a
  **connection-independent liveness signal** — the launcher `DHCPACK` (or the
  vsock agent ping) is the right instrument; the serial console is not.

**Correction to the diagnosis above:** the "froze after
`MpInitChangeApLoopCallback() done!`" evidence was over-read. In the off-cluster
harness that line is the firmware's **normal** hand-off point — the firmware
debug port goes quiet there on **every** boot (successful or not) because the
kernel takes over the console. So "last line is `MpInit...done`" is **not**, by
itself, evidence of an AP-init firmware hang; the true stall point is the kernel
initramfs on the bad-geometry disk (see "ROOT CAUSE").

## Related

- The fix: `maybeRootDiskFromSourceClone` in
  [`internal/controller/swiftguest/clone.go`](../../internal/controller/swiftguest/clone.go)
  (PR #323).
- The lease-poller-stays-alive fix (v0.4.3): `rust/swiftletd/src/lease.rs`
  (`LEASE_POLL_ATTEMPTS_RESTORE`). Correct and necessary; now the clone actually
  re-DHCPs on reboot so the poller has something to surface.
- Operator runbook caveat:
  [`docs/snapshots/clone-from-snapshot.md`](../snapshots/clone-from-snapshot.md)
  "Known limitation on Cloud Hypervisor v52" — update to reflect the fix.
- The resume-vs-boot identity-inheritance rule (Snapshot Phase 2) is the root
  reason a reboot is needed at all; the in-guest vsock identity agent (v0.4.4)
  removes the need to reboot for identity.
