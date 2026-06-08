# SwiftConfidential — SEV-SNP Confidential VMs

> Status: DESIGN (pre-spike), **hardware-gated**. CH v52.0 can launch AMD SEV-SNP
> confidential VMs on KVM; this scopes confidential VMs as a first-class KubeSwift
> workload. No SEV-SNP hardware exists on the dev cluster, so this is design-first
> — validated when an AMD EPYC SEV-SNP host is available. Last updated: 2026-06-08.

## 1. Goal

Run **confidential VMs** — guests whose memory is hardware-encrypted and
integrity-protected so the **host, hypervisor, and cluster operator cannot read
or tamper with guest memory** — as SwiftGuests, with **measured boot** and
**remote attestation** so a relying party can prove *what* booted before
releasing secrets to it. This is a potential major differentiator: confidential
computing as a turnkey Kubernetes-native VM workload.

Out of scope for v1: confidential GPU (SEV-SNP + GPU passthrough), live migration
of confidential VMs, and snapshots of confidential VMs (all involve encrypted
guest state and are hard problems — future phases).

## 2. What SEV-SNP gives us

AMD **SEV-SNP** (Secure Encrypted Virtualization — Secure Nested Paging):

- **Memory encryption** — each guest's RAM is encrypted with a per-VM key held in
  the AMD Secure Processor (PSP/ASP); the host/hypervisor see only ciphertext.
- **Integrity (SNP)** — a Reverse Map Table (RMP) prevents the host from
  remapping, replaying, or corrupting guest pages.
- **Measured boot** — the initial guest image (firmware, kernel, cmdline, initrd)
  is measured into a launch digest by the PSP.
- **Attestation** — the guest can request a PSP-signed **attestation report**
  containing the launch measurement + a guest-supplied `report_data` (nonce). A
  verifier checks the AMD signature chain + the expected measurement before
  trusting the guest (e.g. releasing a key).

The trust boundary moves: **KubeSwift, swiftletd, the kernel, and the cluster
admin are all OUTSIDE the guest's trust boundary.** This changes our threat model
(§8) and what status we can honestly report.

## 3. CH v52 SEV-SNP launch model (the building blocks)

From the CH v52.0 release (#7942, #8123), a SEV-SNP guest on KVM uses:

- **`guest_memfd`** — KVM-private memory backing the encrypted guest (the host
  cannot map it). CH allocates guest RAM via `guest_memfd` for SNP guests.
- **IGVM firmware** — the guest is brought up from an **IGVM** (Independent Guest
  Virtual Machine) package, e.g. **Oak stage0** or an OVMF-IGVM build, passed via
  `--firmware`. IGVM carries the initial memory image + the directives the PSP
  measures.
- **Measured launch** — kernel, command line, and initrd are reflected in the
  launch measurement (parity with the QEMU SNP flow).
- **Signed ID block** — a signed SNP **ID block** can be passed so the guest (or a
  remote attestor) can verify the launch against an expected identity/author key.

> **Prerequisite finding (grounded):** the shipped `cloud-hypervisor-static`
> binary's `--help` exposes `--firmware` and `--platform` but **no SEV-SNP-specific
> flags** — SEV-SNP is behind a Cargo build feature (`sev_snp`). So **a SEV-SNP-
> enabled CH build is a prerequisite**: the `swiftletd` image would need a
> CH binary compiled with `--features sev_snp` (and possibly an MSHV vs KVM build
> consideration), separate from the default static binary we ship today. This is
> the first thing the spike must produce. The exact `--platform`/payload CLI for
> SNP (host_data, ID block path, IGVM measurement inputs) is enumerated against
> that build during the spike — this doc does not guess the flag spelling.

## 4. Node requirements & discovery

A confidential node needs: an **AMD EPYC** CPU with **SEV-SNP** enabled in BIOS,
a host kernel with SNP + `guest_memfd` support, the AMD PSP/`sev` device, and the
SEV-SNP-featured CH binary in the launcher image.

- **Node opt-in label:** `kubeswift.io/confidential-node=true` (mirrors the GPU /
  kernel node opt-in pattern).
- **Discovery:** a lightweight extension of the existing discovery DaemonSet (or a
  new `SwiftConfidentialNode` cluster CRD, mirroring `SwiftGPUNode`) surfaces:
  SNP availability, PSP firmware version, the max SNP ASIDs (the hardware cap on
  concurrent SNP guests), and the host kernel/CH SNP-build readiness — as a
  `status.confidentialReady` condition the scheduler/webhook pre-flights (the
  same pattern as `SwiftGPUNode.status.vfioReady`).

## 5. CRD surface — decision

Two shapes were considered (mirroring the GPU model's two-resource split):

- **(A) A `spec.confidential` block on SwiftGuest (+ SwiftGuestClass default).**
  Confidential is a *property of the guest*, not a shared device pool — there is
  nothing to "allocate" the way GPUs are allocated from a node inventory. A spec
  block is the natural fit and mirrors `osType` / `storage`.
- **(B) A dedicated `SwiftConfidentialProfile` CRD** (like `SwiftGPUProfile`),
  referenced by `confidentialProfileRef`. Better if confidential acquires a rich,
  reusable policy surface (attestation endpoints, allowed measurements, key
  brokers) shared across many guests.

**Recommendation: start with (A)**, a `spec.confidential` block, and promote to a
profile CRD (B) only if the attestation-policy surface grows. Sketch:

```yaml
spec:
  confidential:
    enabled: true                 # gate; default false (no behaviour change)
    type: sev-snp                 # +kubebuilder:validation:Enum=sev-snp (room for tdx later)
    # Measured-boot inputs the launch measurement covers (kernel boot path):
    #   the guest MUST boot via kernelRef (measured) — disk boot's bootloader
    #   chain is not measured by the IGVM launch digest. See gating in section 6.
    attestation:
      idBlock:                    # optional signed SNP ID block
        secretRef: { name: snp-id-block }   # author key + expected id; never inline
      # hostData is a 32-byte operator value bound into the report (e.g. a policy hash).
      hostData: ""
    # policy bits (debug disabled, migration disabled, SMT policy) — SNP guest policy.
    policy:
      allowDebug: false
```

Webhook rules (per-operation discipline, Principle #10): `confidential.enabled`
requires `type: sev-snp`; rejects `gpuProfileRef` (no confidential GPU in v1);
requires a measured boot source (§6); rejects live-migration/snapshot opt-ins on
the guest (§9). Secrets (ID block, keys) are **always** `secretRef`, never inline
(Principle: credentials in Secrets, never annotations/specs/logs).

## 6. Runtime — RuntimeIntent + swiftletd

- **Resolver/RuntimeIntent:** a `Confidential` intent block (type, IGVM firmware
  path, ID-block path, host_data, policy) flows to swiftletd, analogous to the
  `GPU` intent.
- **swiftletd / swift-ch-client:** a new spawn path builds the SEV-SNP CH
  invocation — `guest_memfd`-backed memory, `--firmware <IGVM>`, the platform/SNP
  payload (ID block, host_data, measurement inputs). This is a distinct
  `VmConfig` shape (no `--disk`-of-unmeasured-rootfs in the measured set;
  encrypted memory backend).
- **Boot path constraint:** the launch measurement covers the IGVM + the
  **kernel/cmdline/initrd**, NOT a disk bootloader chain. So v1 confidential
  guests boot via **`kernelRef`** (the measured kernel+initramfs path KubeSwift
  already has) — disk boot's GRUB/bootmgr chain isn't in the measurement and
  would defeat attestation. The webhook enforces this.
- **Firmware:** ship/reference an IGVM firmware (Oak stage0 or OVMF-IGVM) in the
  launcher image, pinned + checksummed like `CLOUDHV.fd`.

## 7. Attestation flow (v1 scope)

KubeSwift's job is to **launch a measured SNP guest and make the measurement
inputs deterministic + operator-controlled**; consuming the attestation report is
guest/relying-party side. v1 scope:

1. The operator supplies the expected measurement inputs (kernel/initrd/cmdline
   are KubeSwift-controlled and reproducible; ID block + host_data via the CRD).
2. The guest, at boot, fetches its PSP-signed attestation report (in-guest agent
   over the standard SNP guest interface) and presents it to a **verifier / key
   broker** (e.g. a KBS — out of KubeSwift's scope) which checks the AMD cert
   chain + expected measurement, then releases secrets.
3. KubeSwift **surfaces** the launch measurement / ID it booted with on
   `status.confidential` (an honest "here is what we measured at launch"), and a
   `Confidential` condition — but KubeSwift is NOT a trust anchor (it's outside
   the guest TCB; §8).

A bundled attestation/KBS integration is a **future phase**, not v1.

## 8. Threat model & security (security-engineer)

- **Host/hypervisor/operator are untrusted** for guest *confidentiality + integrity*
  (that's the whole point). KubeSwift status fields about the guest's *internal*
  state are therefore advisory only — the authoritative signal is the
  PSP-signed attestation the guest itself produces.
- **What KubeSwift still controls** (and must do correctly): the measurement
  *inputs* (firmware/kernel/cmdline/initrd pinning), the SNP **policy** (debug
  off, migration off), keeping the ID block/keys in Secrets, and node
  attestation-readiness pre-flight.
- **Availability, not confidentiality, is KubeSwift's remaining lever** — a
  malicious host can deny service (it can't decrypt). Document this boundary
  prominently so operators don't over-trust controller status.
- Composes with CH v52's **vCPU core-scheduling** (SMT side-channel mitigation) —
  a confidential SwiftGuestClass should default SMT-safe scheduling.

## 9. Interactions (out of scope for v1)

- **Confidential + GPU** — confidential GPU needs SEV-SNP + trusted IO (TDISP /
  the modern iommufd/vfio path). Rejected by the webhook in v1.
- **Live migration** — SNP guest migration requires encrypted-state transfer +
  re-attestation; not in v1 (the migration webhook rejects confidential guests).
- **Snapshots** — encrypted memory snapshots are a hard problem; rejected in v1.

## 10. Open questions / spike plan

When EPYC SEV-SNP hardware exists, the spike answers:

1. **Produce a SEV-SNP CH build** (`--features sev_snp`) and confirm the exact
   `--platform`/payload CLI for the IGVM firmware, ID block, host_data, and
   measurement inputs (§3 prerequisite).
2. Launch a measured SNP guest end-to-end (kernel boot) and fetch a PSP-signed
   attestation report from inside the guest; confirm the launch measurement is
   reproducible across boots.
3. Confirm `guest_memfd` + the host kernel/PSP versions required; settle the node
   discovery fields (max ASIDs, PSP version).
4. Decide CRD shape (A vs B) once the attestation-policy surface is concrete.

Until then: **design-complete, asset-gated** — shipped as such, like Tier 2/3 GPU.

## 11. Phased PR breakdown (provisional — after the spike)

| PR | Scope |
|---|---|
| 0 | Spike: SEV-SNP CH build + measured-launch + attestation on real EPYC hardware. |
| 1 | This design doc. |
| 2 | `spec.confidential` CRD block + webhook (measured-boot/no-GPU/no-migration rules) + resolver wiring. |
| 3 | Node opt-in label + discovery (`confidentialReady` + SNP capacity) + pre-flight. |
| 4 | Runtime: SEV-SNP CH spawn path (guest_memfd + IGVM + ID block) in swiftletd; IGVM firmware in the launcher image. |
| 5 | `status.confidential` (launch measurement / ID) + `Confidential` condition + threat-model docs. |
| (future) | Attestation/KBS integration; confidential GPU; confidential migration/snapshots. |

🤖 Generated with [Claude Code](https://claude.com/claude-code)
