# Cloud Hypervisor v52.0 — Capabilities Assessment for KubeSwift

> Status: ASSESSMENT (2026-06-07). Triggered by the v51.1 → v52.0 bump (PR #150).
> Maps the v52.0 release ([notes](https://github.com/cloud-hypervisor/cloud-hypervisor/releases/tag/v52.0),
> published 2026-05-14) onto KubeSwift's documented workarounds and roadmap. v51.2
> was a CVE-only point release, so **v52.0 is the entire delta from v51.1**.
>
> Each item is tagged with the KubeSwift workaround / tracked-follow-up (TFU) / bug
> it touches, and a disposition. Nothing here is shipped yet — this is the backlog
> the bump unlocks. Verified against the code where a removal is claimed.

## TL;DR — the high-value items

1. **`image_type=raw` on `--disk`** — v52.0 **deprecates disk image-type
   auto-detection**; we rely on it. Add it now (forward-compat) — and it likely
   resolves **W10** (the boot-time `ReadOnly`-at-sector-0 WARN). *Quick win.*
2. **Auto-resume on restore** (#7857) — **removes the `resumeCloneIfNeeded`
   workaround (Bug #73)** for `cloneFromSnapshot`. *Quick win.*
3. **Downtime observability + user-configurable downtime** (#7979, #7835) — likely
   **resolves W28** (true vCPU stop-the-world metric), informs **TFU #11**
   (`observedPauseWindow` rename), and may **reshape the migration convergence
   model** (Phase 2 Decision 4's hardcoded 5-iteration cap). *Spike.*
4. **Sparse memory snapshots (#8113) + userfaultfd demand-paged restore (#7800)** —
   smaller snapshots (S3/local) and far lower restore/clone/cutover latency for
   large guests. Revisits the **PR #118** memory-ranges assumption. *Adopt.*
5. **CVE-2026-45782 + guest→VMM panic hardening** — the bump is **security-motivated
   on its own**. *Security review.*
6. **SEV-SNP confidential VMs on KVM** — a potential **major new KubeSwift
   capability** (confidential workloads). *Roadmap.*

---

## 1. Workaround removals & improvements (mapped to our backlog)

### 1.1 `--disk image_type=raw` — autodetection deprecation (+ likely W10 fix) — QUICK WIN
- **v52.0:** "Auto-detection of disk image types is now **deprecated** and will be
  removed in a future release. Specify `--disk image_type=...`" (#8219). Also
  "Block devices unconditionally assume sparse support" (#7757) and "Disable
  sector 0 writes for autodetected VHD images" (#8218).
- **KubeSwift today:** `swift-ch-client/src/config.rs` builds `--disk path=<...>`
  with **no `image_type`** (root, seed, data, restore disks) — we depend on
  autodetection. On v52.0 we'll emit deprecation warnings, and break when it's
  removed.
- **W10 link:** the tracked `Request check failed: ... ReadOnly` WARN at **sector 0**
  during early boot of Block-mode guests is consistent with CH's image-type
  **autodetection probe** touching sector 0. Passing `image_type=raw` skips the
  probe entirely and should remove the WARN.
- **Disposition:** add `image_type=raw` to every `--disk` we emit (raw is our
  runtime invariant — Design Principle #3). Forward-compat + probable W10
  resolution. Confirm W10 is gone in the v52 Linux regression. **Low effort.**

### 1.2 Auto-resume on restore (#7857) — removes `resumeCloneIfNeeded` (Bug #73) — QUICK WIN
- **v52.0:** "A new option to **automatically resume the VM on restore**, useful
  when restoring from the VMM command line **without an API socket**" (#7857).
- **KubeSwift today:** `spawn_ch_restore` passes `--restore` and the **caller drives
  `resume`**. For `cloneFromSnapshot` there is no SwiftRestore to drive it, so Bug
  #73 added `resumeCloneIfNeeded` (controller sends a one-shot
  `kubeswift.io/snapshot-action: resume` once the clone pod is Running) — a
  controller round-trip purely to unpause a `--restore`'d clone.
- **Disposition:** pass the auto-resume option on the clone restore path → CH
  resumes itself → **delete `resumeCloneIfNeeded`** and its annotation round-trip
  (lower latency, less code). The SwiftRestore in-place/clone-receive path keeps its
  controller-driven Resuming phase (it needs the status transition), but the
  `cloneFromSnapshot` path is the clean win. **Low effort.**

### 1.3 Downtime observability + user-configurable downtime/timeout (#7979, #7835) — SPIKE
- **v52.0:** "User-configurable **downtime** and **timeout** parameters for live
  migration (#7835), and **improved downtime observability** (#7979)."
- **KubeSwift today:**
  - **W28 / W27b / TFU #11:** `status.observedPauseWindow` measures the *entire*
    `send-migration` RPC (pre-copy + stop-and-copy + finalize), **not** the actual
    vCPU stop-the-world. W28 explicitly listed "future CH versions may grow
    per-phase timing on the `vm.send-migration` response" as the cleanest fix path.
    #7979 is plausibly exactly that — letting us report a **true** downtime metric
    and finally rename `observedPauseWindow` → `transferDuration` (TFU #11) with a
    separate real-pause field.
  - **Phase 2 Decision 4:** our migration spec (`spec.maxPauseWindow`, dirty-rate
    admission) was **designed around CH v51.1's hardcoded 5-iteration pre-copy
    cap**. The context doc flagged: *"future CH versions making the cap
    configurable or replacing it with classical dirty-rate-vs-bandwidth detection
    would change Phase 3b's webhook policy."* #7835 (configurable downtime target)
    may be that change — convergence-to-target instead of a fixed iteration count.
- **Disposition:** **spike the v52 `send-migration` API surface** (request params:
  `downtime`, `timeout`, `connections`; response body: per-phase timing) on the
  cluster; then re-plumb `swift-ch-client::send_migration` + the controller's
  pause-window/downtime stamping and the webhook admission policy. Non-blocking
  Phase 3 follow-up; meaningful operator-facing quality.

### 1.4 Sparse snapshots (#8113) + userfaultfd restore (#7800) — ADOPT
- **v52.0:**
  - **Sparse memory snapshots** — `SEEK_DATA`/`SEEK_HOLE` skip untouched pages on
    snapshot, read sparse on restore; "substantially reducing both snapshot size
    and restore time" (#8113).
  - **Userfaultfd demand-paged restore** — `memory_restore_mode` populates guest
    memory lazily instead of reading the whole snapshot before resume;
    "dramatically reduces restore-to-resume latency for large guests" (#7800).
- **KubeSwift today:** Tier B/C snapshots store the **full** guest-RAM memory file;
  restore reads it all before resume (the "~2.8 s/GiB pause window"). This is the
  cost driver for S3 upload size (Tier C), local snapshot disk (Tier B), and
  restore/clone/cutover latency.
- **PR #118 interaction:** our S3 dedup assumed "the memory-ranges file is **always
  exactly guest RAM size**" and skipped re-upload on size+sha match. With **sparse**
  snapshots the file is smaller/sparse — the premise changes (the sha256 check
  still holds; the size heuristic and the bug's framing should be revisited).
- **Disposition:** adopt `memory_restore_mode=userfaultfd` on restore (snapshot
  restore, `cloneFromSnapshot`, and the live-migration receive path — it cuts
  cutover downtime for large guests); verify sparse-snapshot output and update the
  PR #118 dedup notes + Tier C size accounting. **High value, snapshot+migration.**

### 1.5 Clock-on-restore correctness (#7932, #7933) — VALIDATE (free win)
- **v52.0:** KVM clock restored **before** vCPUs resume (#7932); `notify_guest_clock_paused`
  for **Hyper-V** guests (#7933) — "eliminating clock jumps observed after restore."
- **KubeSwift:** no explicit workaround, but restored/migrated guests could exhibit
  clock skew; the Hyper-V path (#7933) matters now that **Windows uses
  `kvm_hyperv=on`**. Free correctness improvement from the bump — confirm in the
  snapshot/migration regression.

### 1.6 Multi-connection migration (#7669) — OPTIONAL throughput
- **v52.0:** `send-migration` accepts `connections` (default 1) for parallel TCP
  transfer; "significantly increase migration throughput on 100G links" (#7669).
- **KubeSwift:** single connection. Phase 3b measured **~95% of raw TCP** on the 1G
  spike link, so the benefit here is small; it pays off on 10G/100G. Fold a
  sensible `connections` default into the §1.3 send-migration re-plumb when adopting
  the v52 API. **Network-dependent; low priority on the current 1G cluster.**

---

## 2. New capabilities — roadmap opportunities (architect)

- **SEV-SNP confidential VMs on KVM (#7942, #8123)** — encrypted guest memory via
  `guest_memfd`, IGVM firmware, **measured boot + attestation**, now on KVM (not
  just MSHV). Potential **major KubeSwift differentiator**: confidential VMs as a
  first-class workload (a `confidential` SwiftGuestClass / SwiftConfidential CRD).
  Needs SEV-SNP-capable AMD EPYC hardware (none on the dev cluster). **Flag to the
  staff-architect as a roadmap candidate.**
- **iommufd / vfio-cdev VFIO (#7981)** — modern Linux VFIO access model; "fully
  accelerated IOMMU support inside the guest." We use the legacy container/group
  path for GPU. Modernization path; enables **in-guest vIOMMU** (relevant to
  nested/confidential GPU and some Tier 2/3 topologies).
- **GPU/VFIO BAR controls (#7991, #7939, #7940)** — `host_mmap_bars` (selective BAR
  mapping) is the **CH-native equivalent** of the QEMU `x-no-mmap=true` we use for
  large-BAR GPUs (B200). It could let some **large-BAR Tier 2/3 GPUs run on CH**
  rather than QEMU. Sub-page BAR expansion (#7939) improves small-BAR reliability
  (IOMMU-group companions, SR-IOV VFs); lazy GSI allocation (#7940) helps many-device
  guests. **Evaluate when GPU hardware is available (security-engineer + rust-runtime).**
- **Generic `vhost-user` device (#7221)** — attach arbitrary vhost-user backends via
  CLI/API. Underpins the future **"vhost-user" SwiftKernel profile** and high-perf
  net/blk/fs backends.
- **`--no-shutdown` (#8025) + reset-in-place (v52 behaviour)** — finer VMM
  lifecycle control. **Reset-in-place** (v52) vs **exit-on-reset** (v51.1, found in
  the Windows spike) is a **behaviour change**: a guest reboot now stays in the same
  CH/pod instead of cycling the launcher pod. This interacts with `runPolicy`
  (RestartOnFailure/Always) and could simplify the controller's restart handling;
  `--no-shutdown` lets the controller fully own the VMM on guest shutdown
  (distinguish shutdown from crash). **Validate the reboot semantics in the Linux
  regression; consider simplifying runPolicy handling afterward.**
- **Core scheduling for vCPU threads (#7747)** — SMT side-channel mitigation without
  disabling SMT; a candidate per-class security option for multi-tenant workloads.

## 3. Security posture (security-engineer)

- **CVE-2026-45782** — use-after-free in the `virtio-block` async I/O completion
  path. **v51.1 is vulnerable**; the bump is security-motivated independent of
  Windows. (v51.2 backported the fix; we go straight to v52.0.)
- **Guest→VMM hardening** — many fixes close guest-triggerable VMM panics / DoS: OOB
  `queue_select` on the virtio-PCI common config (#7918), IOMMU translation errors
  no longer panic (#8023), virtio error paths reset queues instead of exiting
  (#8128, #8186), `CpuManager::pause()`/ACPI-hotplug deadlock (#7990, #8092), balloon
  underflow/oversize validation (#7903, #8116). These strengthen the **guest ↔
  launcher-pod isolation** boundary central to KubeSwift's multi-tenant model.

## 4. What v52.0 does NOT address — keep these workarounds

- **Migration mTLS** — no native migration TLS in v52.0. **Keep the Phase 3c stunnel
  sidecar.**
- **Blocking `send_migration` on the single-threaded action loop (TFU-14 / W12)** —
  a swiftletd architecture issue (`current_thread` runtime + sync blocking HTTP),
  not a CH gap. The v52 `connections`/`downtime` params don't make the call
  non-blocking. **The async/worker-thread refactor stays a swiftletd TODO** (though
  configurable downtime can shorten the window).
- **VFIO snapshot restore (Snapshot Constraint #1)** — the notes don't claim VFIO
  restore is fixed; `iommufd` is a new *access model*, not a stated restore fix.
  **Assume still blocked until tested.**
- **Snapshot identity regen without reboot** — still needs an in-guest agent
  (vsock). v52's "vsock reset on restore" (#7958) helps such an agent but doesn't
  regenerate identity.

## 5. Recommended actions (prioritized)

| # | Action | Touches | Effort | When |
|---|---|---|---|---|
| 1 | `--disk image_type=raw` on all disks | autodetection deprecation #8219; **W10** | Low | Fast-follow to the v52 bump |
| 2 | Auto-resume-on-restore for `cloneFromSnapshot`; remove `resumeCloneIfNeeded` | #7857; **Bug #73** | Low | After bump deploys |
| 3 | Validate reset-in-place + clock-restore behaviour; W10 gone | #7932/#7933; runPolicy; **W10** | Low | In the v52 Linux regression |
| 4 | Spike v52 `send-migration` API (downtime obs + configurable downtime + connections) | **W28, W27b, TFU #11**, Phase 2 Decision 4 | Med | Phase 3 follow-up |
| 5 | Adopt userfaultfd restore + sparse snapshots; revisit PR #118 dedup | #7800/#8113; **PR #118**; restore latency | Med | Snapshot/migration follow-up |
| 6 | Roadmap eval: SEV-SNP, iommufd/`host_mmap_bars` (GPU), generic vhost-user, core-scheduling | new capabilities | — | Architect/roadmap |
| 7 | Security review of the CVE + guest→VMM hardening | security posture | Low | security-engineer |

Items 1–3 are concrete, low-effort, and naturally ride alongside the v52 bump
validation. Items 4–5 are the higher-value follow-ups. Item 6 is roadmap input.
None blocks the Windows `osType` work (PR 2), which is hypervisor-independent.

🤖 Generated with [Claude Code](https://claude.com/claude-code)
