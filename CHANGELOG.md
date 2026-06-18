# Changelog

All notable changes to KubeSwift are documented here.

---

## [Unreleased]

---

## [v0.4.6] — 2026-06-18

Adds **OVN-Kubernetes as a second OVN CNI backend**: a guest's **primary** NIC on an
OVN-Kubernetes `layer2` NAD gets a portable IP with **IP-preserving live migration** —
the same capability the kube-ovn backend already delivers, now on clusters where
OVN-Kubernetes is the primary CNI. Cluster-validated end-to-end on a kubeadm
OVN-K-primary cluster (boot + `mode: live` migration, no `allowIPChange`, IP
preserved). Also hardens controller startup on clusters that lack the CSI
VolumeSnapshot CRDs, and ships a full operator-doc set (cluster setup + per-tenant
multi-tenancy recipes for both OVN substrates).

### Added
- **OVN-Kubernetes as a second OVN CNI backend (OVN-K arc P2).** `ovnKubernetesBackend`
  implements the `ovnBackend` seam for OVN-Kubernetes **layer2 primary-on-NAD** guests:
  it detects `type: ovn-k8s-cni-overlay` + `topology: layer2`, injects the guest MAC
  (the logical-switch-port identity) + an `ipam-claim-reference` into the Multus
  network-selection element, and creates+owns a per-guest `IPAMClaim` to pin the
  primary IP (OVN-K does not auto-create it). The identity mechanism — the `mac`
  field of the selection element — was confirmed on a real OVN-K cluster (a foreign
  MAC requested there comes up reachable cross-node). Live-migration IP preservation
  rides the already-carried-over Multus annotation: OVN-K allows the cross-node claim
  overlap by **default**, so no `migrationJobName`-style marker is needed (simpler
  than kube-ovn). Adds `ipamclaims` RBAC (`get,list,watch,create`; GC via owner-ref
  cascade). The `PrimaryIPPreservedCrossNode()` live-eligibility gate already covers
  it (CNI-agnostic). **Cluster-validated end-to-end** on a real OVN-Kubernetes-primary
  cluster: a RWX+Block disk-boot guest on a `layer2 allowPersistentIPs` NAD booted
  reachable cross-node, then `mode: live`-migrated with **no `allowIPChange`** in
  **2.8 s** with the IP preserved and reachable from a third node. Operator guide:
  [`docs/networking/ovn-kubernetes-install.md`](docs/networking/ovn-kubernetes-install.md).
  UDN-*secondary* networks ride this backend transparently (a generated `layer2` NAD);
  UDN-*primary* multi-tenancy is a separate later phase. ([RFC](docs/design/ovn-cni-backends.md))

### Changed
- **Internal: pluggable OVN CNI backend seam (OVN-K arc P1).** Lifted the kube-ovn
  primary-on-NAD identity logic behind an internal `ovnBackend` interface
  (`internal/controller/swiftguest/ovn_backend.go`) — `Detect`/`Identity` per
  backend, first-match-wins, "two implementations, not a framework". The two call
  sites (the launcher-pod stamp and the live-migration dst-pod annotations) now
  dispatch through the seam, so additional OVN-based CNIs (OVN-Kubernetes next)
  plug in without touching the controller. **Behavior-preserving** — no
  user-facing change; the shipped kube-ovn IP-preserving live-migration path is
  identical and its tests pass unchanged. Foundation for OVN-Kubernetes support
  ([RFC](docs/design/ovn-cni-backends.md)).

### Fixed
- **Controller no longer crash-loops on a cluster without the CSI VolumeSnapshot
  CRDs.** The snapshot controllers `Owns(VolumeSnapshot)`; when the
  external-snapshotter CRDs (`snapshot.storage.k8s.io/v1`) are absent, that watch's
  cache could never sync and the manager exited fatally — so on a bare cluster
  (e.g. one whose CSI driver doesn't bundle them) KubeSwift never started. The
  manager now does a one-time discovery check and **gates those watches** on it,
  logging a clear warning and degrading gracefully: the **core VM runtime**, the
  **local/s3 snapshot backends**, and `cloneStrategy=copy` all keep working; only
  the **CSI VolumeSnapshot backend** and `cloneStrategy=snapshot` are disabled
  until the CRDs are installed. Surfaced by the OVN-K P3 cluster validation (the
  W5 pattern — a bare cluster exposes a hard dependency unit tests can't see).

### Docs
- **OVN-Kubernetes + UDN operator guides.** A kubeadm + OVN-Kubernetes-primary
  cluster setup guide ([`kubeadm-ovn-kubernetes-setup.md`](docs/networking/kubeadm-ovn-kubernetes-setup.md)),
  the OVN-Kubernetes-primary install guide
  ([`ovn-kubernetes-install.md`](docs/networking/ovn-kubernetes-install.md)), and
  per-tenant multi-tenancy recipes for both OVN substrates
  ([`udn-multi-tenancy.md`](docs/networking/udn-multi-tenancy.md) for OVN-K,
  [`kubeovn-multi-tenancy.md`](docs/networking/kubeovn-multi-tenancy.md) for kube-ovn).

---

## [v0.4.5] — 2026-06-17

Closes the **multi-node L2 / kube-ovn primary-on-NAD** arc: a guest whose
**primary** NIC rides a kube-ovn NAD is reachable cross-node on its own IP and
**preserves that IP across a `mode: live` migration with no `allowIPChange`** —
zero-touch, no manual `ovn-nbctl`. Cluster-validated end-to-end on image
`sha-e403f4c`.

### Added

- **kube-ovn primary-on-NAD integration (IP-preserving live migration on a real
  Tier-C OVN L2).** When a SwiftGuest's **primary** interface rides a
  kube-ovn-class NAD (`config.type: kube-ovn`), KubeSwift now programs the guest's
  identity onto the OVN logical-switch port so the guest is reachable on the
  segment, and preserves its IP across a live migration — with **no manual
  `ovn-nbctl`**. Two coupled pieces:
  - **Controller (#239):** stamps `<provider>.kubernetes.io/mac_address: <guest MAC>`
    on the launcher pod (so OVN's per-port ARP responder / L2 delivery target the
    guest's bridged MAC, not the pod NIC's) and, once known,
    `<provider>.kubernetes.io/ip_address: <guest IP>` (a stable static IP across
    pod recreate). The live-migration **destination** pod additionally gets
    `kubevirt.io/migrationJobName`, which makes kube-ovn's IPAM skip the conflict
    check so the dst acquires the **same** static IP the source still holds through
    cutover. Reads NADs read-only (new
    `k8s.cni.cncf.io/network-attachment-definitions` get RBAC). No-op for every
    other networking mode (node-local bridge, non-kube-ovn NAD, SR-IOV). Composes
    with `#235`/`#236` (the NAD live-migration carry-through).
  - **Launcher datapath fix (this change):** stamping the guest MAC onto the pod
    NIC means the NIC and the guest's tap share a MAC; enslaving the NIC to `br0`
    makes the kernel add a permanent fdb entry `<guest-mac> -> NIC` that **shadows
    the tap** (the bridge sends the guest's return traffic to the NIC, not the
    guest). `network-init` now re-MACs the NIC to a dummy **before** enslaving it
    (the KubeVirt bridge-binding pattern); the OVN port keeps the guest MAC, so OVN
    still delivers the guest's frames `NIC -> br0 -> tap`. A no-op for any NAD whose
    IPAM gives the NIC its own distinct MAC.
  - **Validation.** Cluster-validated **zero-touch end-to-end** on image
    `sha-e403f4c`: a fresh kube-ovn-primary guest auto-stamps its OVN port
    identity and `network-init` auto-re-MACs the pod NIC, so the guest comes up
    reachable cross-node with its IP in `status.network.primaryIP` (no manual
    `ovn-nbctl`); a cross-node `mode: live` migration with **no `allowIPChange`**
    Completed in **~3.2 s** downtime with the IP **preserved and reachable from a
    third node**. The post-merge cluster validation of the **automated** path
    surfaced the NIC-MAC-shadow gap above (the W5 pattern: unit tests verify the
    annotation, only a cluster exercises the bridge fdb) — fixed here. **Both**
    the controller and the launcher image must carry the integration.

### Changed

- **De-experimentalized the multi-node L2 guide (#238).**
  [`docs/networking/multi-node-l2.md`](docs/networking/multi-node-l2.md) now
  carries an honest validation matrix: the primary-on-NAD datapath, offline
  migration, and **live** IP-preservation are cluster-validated on a kube-ovn
  Tier-C L2; the hand-rolled bridge/macvlan mesh validates the datapath and
  offline moves but not live migration.

### Fixed

- **Live migration of a guest on a Multus NAD no longer fails `DstNeverReady`
  (#235).** The migration **destination** pod now inherits the source's
  `k8s.v1.cni.cncf.io/networks` request annotation, so its secondary interface
  (`net1`) is plumbed by Multus before the receiver starts — previously the dst
  pod came up without the NAD attachment and the migration timed out waiting for
  a receiver that could never bind.
- **Bounded send-retry when the destination CH receiver is not yet listening on
  the migration channel (#236).** The source launcher now retries the
  live-migration send for a short bounded window instead of failing the first
  time the dst Cloud Hypervisor receiver has not finished binding the channel —
  closing a cutover race that intermittently failed otherwise-healthy
  migrations.
- **Hardened the primary-on-NAD per-pod dnsmasq for a shared flat multi-node L2
  (#237).** On a NAD that places every guest on one flat L2 segment, the per-pod
  dnsmasq now answers **only its own guest's MAC** (it will not reply to a peer
  guest's DHCP on the shared wire), hands out an **infinite lease**, and carries
  the **overlay MTU** so the guest matches the segment.

---

## [v0.4.4] — 2026-06-16

Feature release. The **in-guest vsock identity agent** — a `cloneFromSnapshot`
clone now regenerates its identity (machine-id / SSH host keys / hostname / MAC)
and re-DHCPs **in place, with no reboot**, sidestepping the Cloud-Hypervisor-v52
clone-reboot firmware hang documented in v0.4.3. Cluster-validated end-to-end.

### Added

- **In-guest identity agent over vsock.** Opt a snapshot **source** in with
  `spec.guestAgent.enabled: true`. The controller attaches a Cloud Hypervisor
  `--vsock` device and (with the `guest-agent` SwiftSeedProfile) delivers a tiny
  static agent binary onto the source's NoCloud seed disk, installed on first
  boot. The agent is captured in the memory snapshot and resumes — alive — in
  every clone. When a clone reaches `GuestRunning`, the SwiftGuest controller
  drives a one-shot regeneration over the host↔guest vsock channel: it
  regenerates the items in `cloneFromSnapshot.regenerate`, sets the per-clone
  guest-visible MAC, and re-DHCPs — all without a reboot. The result is reported
  on the new **`CloneIdentityRegenerated`** status condition (`True`, or `False`
  with reason `GuestAgentUnreachable` when the agent is absent — a loud,
  never-silent fallback), and each clone's own IP lands in
  `status.network.primaryIP` via the restore lease-poller.
  - New `cmd/kubeswift-guest-agent` (static Go AF_VSOCK listener; one
    `regenerate-identity` op; validated argv inputs; primary-interface
    detection), embedded in the swiftletd image and delivered via the seed disk.
  - New `rust/swift-vsock-client` crate (the host-side CONNECT-handshake client)
    + a swiftletd `identity` action namespace driving it.
  - `SwiftGuest.spec.guestAgent.enabled` (opt-in on the source; Linux only).
  - Design: [`docs/design/clone-identity-vsock-agent.md`](docs/design/clone-identity-vsock-agent.md);
    operator guide:
    [`docs/snapshots/identity-regeneration.md`](docs/snapshots/identity-regeneration.md)
    + [`docs/snapshots/clone-from-snapshot.md`](docs/snapshots/clone-from-snapshot.md).
  - Cluster-validated on **Tier B** (local) memory snapshots — a 2-clone fan-out
    came up with distinct machine-id / hostname / MAC / IP per clone, no reboot.
    **Tier C** (S3) clones inherit the identical agent flow (the snapshot carries
    the agent + vsock device regardless of backend); validate in your environment.

### Notes

- No new container image or chart toggle: the agent ships inside the `swiftletd`
  image and the opt-in is a SwiftGuest field. The `swift.kubeswift.io` CRD gains
  `spec.guestAgent`; `kubectl apply`/`helm upgrade` updates it.
- Windows guests (`osType: windows`) are out of scope for the agent in this
  release (the regeneration mechanics differ); the controller skips the device.

---

## [v0.4.3] — 2026-06-15

Patch release. swiftletd lease-poller fix for cloneFromSnapshot guests, and a
documented Cloud-Hypervisor-v52 limitation surfaced validating the instant-clone
flow.

### Fixed

- **swiftletd: the lease poller no longer gives up on restore/clone guests.** A
  `cloneFromSnapshot` guest boots via CH `--restore` — it RESUMES the source's
  captured RAM (including its already-configured `eth0` lease) and does not
  re-run DHCP on start, so a fresh clone has no lease to discover. The guest only
  re-DHCPs on a later reboot, but the lease poller capped at ~4 min and then
  exited permanently (`lease_poll_timeout`) — so any post-reboot lease landed
  into a dead poller and the IP never reached `status.network.primaryIP`. The
  poll cap is now parameterized: fresh boots keep the ~4 min cap, CH `--restore`
  receivers (`intent.is_restore()`) keep polling for the pod's lifetime so the
  eventual lease is discovered. The poller still terminates immediately on the
  first successful patch. swiftletd-only; no controller/CRD change.

### Known issues

- **A cloneFromSnapshot (memory-snapshot) guest cannot reboot on Cloud
  Hypervisor v52** — rebooting a `--restore`d guest hangs in UEFI firmware (the
  EDK2 S3-resume / AP-init path freezes after `MpInitChangeApLoopCallback`),
  while a *normal* guest reboots through the same point and re-DHCPs in seconds.
  Because the documented clone-identity remedy is "reboot once to regenerate
  identity + re-DHCP", this means memory-snapshot clones currently keep the
  source's guest-visible identity and do not surface their own IP — treat them as
  warm, read-mostly replicas (collision-safe via per-pod network namespaces). It
  is a CH-`--restore`+reboot firmware interaction, not a KubeSwift defect; the
  in-guest vsock identity agent (regenerate without a reboot) is the planned real
  fix. Investigation:
  [`docs/design/known-issues-clone-reboot-firmware-hang.md`](docs/design/known-issues-clone-reboot-firmware-hang.md);
  operator note in
  [`docs/snapshots/clone-from-snapshot.md`](docs/snapshots/clone-from-snapshot.md).

---

## [v0.4.2] — 2026-06-14

Code + chart release. Headline: **blank / raw VM data disks** — the
"give me a sized empty volume for my database" case that previously had no
first-class option — plus five fixes surfaced dogfooding the demo flows.

### Added

- **Blank / raw VM data disks (`spec.dataDiskRefs[]`).** A guest can now attach
  sized, image-less block disks that the guest formats itself — the previously
  missing empty-volume primitive.
  - `dataDiskRefs[].blank: {size, storageClassName, volumeMode}` provisions a
    guest-owned PVC (Block by default), attached to the VM as a raw `--disk`
    (CH `--disk path=/dev/...`); the guest formats and mounts it.
  - `dataDiskRefs[].attachAsDisk` attaches an **existing** Block PVC as a raw VM
    disk (vs the default filesystem-directory mount).
  - The plural `dataDiskRefs[]` is now fully real for VM disks across all three
    kinds — blank, image-backed (`imageRef`), and attached-PVC — and the
    previously dead-code `dataDiskRefs[].imageRef` now works.
  - New `status.dataDisks[]` echo (PVC / volumeMode / devicePath / bound) and a
    **`DataDisksReady`** condition: a guest never boots with a missing data disk
    — it holds in `Scheduling` and names the blocker (Principle #6).
  - Admission validation for `dataDiskRefs[]`: exactly one of
    `imageRef`/`pvcRef`/`blank` per entry, `blank.size > 0`, unique DNS-label
    names, max 8 disks, `attachAsDisk` only with `pvcRef`. Data disks compose
    with GPU.
  - Cluster-validated end to end (blank Block 20Gi: controller → Block PVC → pod
    `volumeDevice` → CH `--disk path=/dev/...` → guest `vdc 20G`; PVC GC'd with
    the guest).
  - Operator guide `docs/api/data-disks.md` + blank-disk sample. **Mount data
    disks by UUID/LABEL** — the in-guest `/dev/vdX` letter is not stable.

### Fixed

- **Snapshot Tier A could capture an empty disk (unbootable restore).** The
  CSI VolumeSnapshot (Tier A) path snapshotted the source guest's root PVC as
  soon as it was `Bound`, but the per-guest rootclone Job writes `image.raw`
  into the PVC *after* it binds — a snapshot taken alongside a fresh source
  guest captured an empty disk, and the restore cloned that empty snapshot.
  The snapshot now gates on the source guest being `Running` or `Stopped`
  (disk populated); Tier B/S3 were unaffected.
- **`swiftctl ssh <guest> -- <command>` now runs a remote command
  non-interactively.** `ssh` was interactive-only and rejected extra args
  (`accepts 1 arg(s)`); it now mirrors `ssh host <cmd>` / `kubectl exec pod --
  <cmd>` — no TTY required, streams stdout/stderr, propagates the remote exit
  code. The bare interactive form is unchanged.
- **The standalone `config/dra-driver/` manifest pinned `:latest`** (a tag the
  registry never publishes), so `kubectl apply` always `ImagePullBackOff`'d —
  and applying it over a Helm install with `dra.enabled=true` clobbered the
  chart's working version-managed image, leaving the DRA driver down and GPU
  ResourceClaims unallocatable. The manifest image is now release-pinned
  (`:v0.4.2`); prefer the chart's `dra.enabled`, which manages the version.

### Changed

- **Performance: the rootclone Job is co-located with the launcher's node.** For
  a node-pinned guest (cloneFromSnapshot with a node-local snapshot,
  `spec.nodeName`, GPU, or migration), an unpinned rootclone Job could populate
  the RWO root PVC on a different node, forcing a ~26s Longhorn detach/reattach
  bounce onto the launcher's node. The Job now carries the launcher's
  `kubernetes.io/hostname` nodeSelector when the target node is known; unpinned
  guests are unchanged.
- Docs: warned against pointing an image-backed data disk at a bootable OS image
  (its partition/FS UUIDs collide with the root disk and corrupt the boot) — now
  superseded by blank disks for the empty-volume case — and corrected the
  data-disk device letter (`/dev/vdc` on disk boot, `/dev/vdb` only on kernel
  boot).

---

## [v0.4.1] — 2026-06-13

Chart-only patch. Identical code and images to v0.4.0 (rebuilt as `v0.4.1`); the
fix is in the Helm chart.

### Fixed
- **The DRA GPU driver is now packaged in the Helm chart.** v0.4.0 shipped the
  DRA GPU allocation backend (`SwiftGuest.spec.gpuResourceClaim`) but its
  reference driver (`kubeswift-dra-driver`) was only available as standalone
  manifests under `config/dra-driver/` — a Helm install could not deploy it, so
  the DRA backend was unreachable on chart-based installs. The chart now ships
  the DRA driver behind a new **`dra.enabled`** toggle (default `false`): the
  DaemonSet on `kubeswift.io/gpu-node=true` nodes, its RBAC, and the
  `kubeswift-vfio-gpu` DeviceClass (`dra.deviceClass.create`, default `true`).
  `dra` is **independent of `gpuDiscovery`** — the DRA driver does its own GPU
  discovery (it publishes ResourceSlices), so a DRA-only cluster runs
  `dra.enabled=true` with `gpuDiscovery.enabled=false`. Enable with
  `--set dra.enabled=true`. The standalone `config/dra-driver/` manifests remain
  for kustomize installs.

---

## [v0.4.0] — 2026-06-13

Everything since v0.3.1 (PRs #198–#223): three feature arcs — **service
exposure**, **DRA GPU allocation**, and an **observability program** — plus the
in-cluster validation of Windows guests and the project's relicensing for
open source. All additive and backward-compatible (`v1alpha1`); no breaking
changes. Each arc shipped with on-cluster validation walkthroughs.

### Highlights

- **Service exposure — VMs as first-class Kubernetes Services (S0–S4).** One
  CNI-agnostic primitive (an in-pod DNAT `podIP:port → vmIP:targetPort`) makes a
  guest port a normal Kubernetes Endpoint, so the whole north-south ecosystem
  composes on top.
  - `SwiftGuest.spec.network.ports[]` exposes guest ports; `expose:
    ClusterIP|NodePort|LoadBalancer` mints one Service per guest with honest
    readiness (the endpoint joins only once the in-guest service answers).
  - `SwiftGuestPool.spec.service` fronts all replicas with **one load-balanced
    Service**; endpoints follow readiness and scale churn (the pool's `scale`
    subresource is the HPA seam). Optional headless variant for sharded serving.
  - A **VM→cluster egress reachability probe** surfaces as `status.network.egress`
    + the `EgressReady` condition — a notorious silent failure made
    `kubectl`-visible.
  - `serviceAnnotations` + `loadBalancerClass` passthrough drive the ecosystem
    (MetalLB, Gateway API, Tailscale, Istio, Linkerd) — recipes in
    `docs/networking/ecosystem-integrations.md`.
- **DRA GPU allocation (Phases 1–2).** GPU passthrough now has **two allocation
  backends** behind one runtime: the native SwiftGPU model (default) and
  Kubernetes **Dynamic Resource Allocation** via `spec.gpuResourceClaim`
  (XOR `gpuProfileRef`). Ships a pluggable two-phase `gpualloc.Backend`, a
  KubeSwift reference DRA driver (`gpu.kubeswift.io`), and the full
  ResourceSlice → scheduler → CDI-env hand-off → CH `--device` chain —
  cluster-validated end-to-end on a real GPU. NVIDIA `k8s-dra-driver-gpu` adapter
  and IMEX/MIG remain hardware-gated.
- **Observability program (O1–O4).** A complete operator observability surface:
  provisioning-native **Grafana dashboards** (six-dashboard taxonomy) gated behind
  a Helm `monitoring.*` block (ServiceMonitor + dashboards), a cache-backed
  **CR-state metrics collector** (11 gauge families), gap-fill counters
  (GPU alloc/release, drain evacuation, image-import outcome, migration
  failure-reason), and a warning-biased **PrometheusRule** starter pack + alerts
  runbook. Fixed two latent metric defects (restart-drift gauge, unbounded label).
- **Windows guests — cluster-validated.** `osType: windows` (introduced in
  v0.3.0) is now validated in-cluster end-to-end: import → CH v52 disk boot
  (`kvm_hyperv=on`, `image_type=raw`) → cloudbase-init over the NoCloud seed →
  Running with DHCP + RDP. Version-aware image-prep tooling for Windows Server
  2022/2025.

### Added

- `SwiftGuest.spec.network` (binding `nat`/`bridge`, `ports[]`,
  `serviceAnnotations`, `loadBalancerClass`); `SwiftGuestPool.spec.service`;
  `status.network.{egress,exposedPorts,serviceRef}` + `PortsProgrammed` /
  `ServiceReady` / `EgressReady` conditions; `SERVICE` / `EGRESS` printcolumns.
- `SwiftGuest.spec.gpuResourceClaim` (DRA backend) + the `kubeswift-dra-driver`
  image.
- Rich `kubectl get` printcolumns for SwiftGuest / SwiftImage / SwiftSeedProfile.
- Helm `monitoring.*` values (ServiceMonitor + dashboard ConfigMaps), shipped
  Grafana dashboards under `config/grafana/`, and a starter `PrometheusRule` pack.
- Operator docs: service exposure + ecosystem integrations, DRA GPU guide,
  fast-VM snapshots/clones guide, live-migratable guests guide, a visual
  architecture reference (Mermaid), and apply-ready samples for all of the above.

### Changed

- **Relicensed to AGPL-3.0** and prepared the repository for open source
  (README rewrite, CRD hygiene, internal development-process docs pruned, dead
  links scrubbed, AI-dev tooling kept local).
- Documentation accuracy pass: corrected the Cloud-Hypervisor-vs-QEMU framing
  (CH is the hypervisor for nearly every workload — Linux **and Windows**, disk
  and kernel boot, PCIe GPU, snapshots, live migration; QEMU is the secondary
  runtime only for HGX SXM multi-GPU topologies), resolved the "Linux only"
  self-contradiction (host x86_64+KVM vs the Linux+Windows guest matrix), and
  brought the CRD reference to all 12 CRDs incl. `spec.osType`. The shipped
  Cloud Hypervisor is **v52.0**.

### Fixed

- DRA cluster-e2e: intent `devices: null` serde, the kubeletplugin socket
  directory, and a hotfix-removal regression.
- Observability: `kubeswift_guest_running_total` restart-drift (now emitted from
  cluster state) and the `vm_failures_total` free-text label (now a bounded
  reason).

---

## [v0.3.1] — 2026-06-10

Patch release. Content is identical to v0.3.0 (images rebuilt as `v0.3.1`);
the fix is in the Helm chart.

### Fixed
- **Helm chart: `migration.mtls.enabled=true` installs were broken** — the
  chart never set `KUBESWIFT_MIGRATION_STUNNEL_IMAGE`, so the controller's
  mTLS sidecar injection fell back to the code default `:latest`, a tag that
  does not exist in the registry. Every launcher pod stuck `1/2
  ImagePullBackOff` (the VM ran, but the guest never reached
  `phase=Running`). The chart now sets the env via a new
  `migrationStunnel.image` values block, mirroring the `snapshotS3` pattern
  (tag defaults to `v<appVersion>`). Found dogfooding the released 0.3.0
  chart — the first helm-path install with mTLS enabled; the kustomize dev
  deploy sets the env via the Makefile, which masked the gap. Installs with
  `migration.mtls.enabled=false` (the default) were unaffected.

---

## [v0.3.0] — 2026-06-09

Consolidates everything since v0.1.0 (the v0.2.0-rc.1 tag from April was never
promoted and is superseded by this release). Roughly 500 commits across six
major feature arcs, each shipped with on-cluster validation walkthroughs.

### Highlights

- **VM snapshots, end to end (Phases 0–6)** — disk-only CSI snapshots,
  local memory snapshots, S3/object-storage export with zstd compression,
  boot-as-clone (`spec.cloneFromSnapshot`), cron scheduling with keep-N
  retention, Prometheus metrics + Grafana dashboard.
- **Live migration, end to end (Phases 1–5)** — offline migration for any
  guest, live migration with sub-3s observed downtime, mTLS-secured
  migration transport, `kubectl drain` integration, offline GPU
  evacuation, metrics + retention.
- **Windows guest support v1** — `osType: windows` on Cloud Hypervisor.
- **vhost-user devices** — virtiofs shared filesystems, vhost-user-net /
  -blk / generic devices (operator-provided backends).
- **Multi-node L2 networking foundation** — primary-on-NAD (experimental)
  and a corrected migration IP-preservation gate; multi-NIC support
  actually works now (three latent bugs fixed; smoke test passes 5/5).
- **Cloud Hypervisor v51.1 → v52.0** — fixes the Windows viostor bugcheck,
  resets guests in place on reboot, unlocks core-scheduling and
  restore/snapshot improvements.

### Added

**Snapshots & restore (`snapshot.kubeswift.io`)**
- SwiftSnapshot + SwiftRestore CRDs and controllers: Tier A CSI
  VolumeSnapshot (disk-only), Tier B local hostPath memory snapshots
  (CH pause/snapshot/resume), Tier C S3-compatible export/import
  (MinIO/AWS/RGW) with checksummed manifests and zstd-compressed memory
  ranges (`spec.backend.s3.compression`).
- `SwiftGuest.spec.cloneFromSnapshot`: boot N guests as clones of one
  memory snapshot (pool-templatable; per-clone hypervisor MAC; CH v52
  auto-resume + on-demand/userfaultfd memory restore).
- `SwiftImage.spec.cloneStrategy: copy|snapshot` for ≥3× faster pool scaling.
- SwiftSnapshotSchedule CRD: cron-created snapshots with `keepLast` pruning,
  reference-aware GC, `spec.ttl`, `spec.deletionPolicy: Delete|Retain` with
  prefix-scoped S3 purge.
- `swiftctl snapshot|restore|schedule` command groups; snapshot metrics,
  byte gauges, Grafana dashboard (`config/grafana/kubeswift-snapshots.json`).

**Live migration (`migration.kubeswift.io`)**
- SwiftMigration CRD + controller: offline mode (direct PVC reuse,
  ~25–70s downtime depending on CSI driver) and live mode (CH pre-copy,
  ~2–3s observed downtime, kernel-boot and RWX+Block disk-boot).
- mTLS migration transport: per-node cert-manager-issued identities,
  SAN-pinned stunnel sidecars (~1% overhead); plaintext path requires an
  explicit unsafe acknowledgement.
- `kubectl drain` integration: eviction webhook + drain controller
  auto-migrate guests per `spec.migration.drainPolicy`
  (Migrate|LiveMigrate|Block); universal per-guest `maxUnavailable: 0`
  PDB as the hard floor; VFIO/GPU guests evacuate offline via the GPU
  release-and-reallocate primitive (reserve-before-stop atomicity).
- Auto mode resolution (live when eligible, else offline), per-guest
  `spec.migration.enabled` pinning, `spec.allowIPChange` opt-in,
  `spec.timeout` (default 30m) and `spec.ttl` retention,
  `status.observedDowntime` / `status.transferProgress` /
  `status.observedTransferDuration`, typed `FailureReasonCode` taxonomy,
  migration metrics + Grafana dashboard.
- `swiftctl migrate` (with `--check` read-only preflight: target
  readiness/capacity, IP preservation, mode resolution, NFD-based CPU
  feature comparison) and `swiftctl migration list|describe|cancel`.

**Windows guests**
- `osType: windows` on SwiftImage/SwiftGuest: CH disk boot with
  `kvm_hyperv=on`, unprivileged import path, cloudbase-init provisioning
  over the existing NoCloud seed, image-prep tooling
  (`tools/windows-image-prep/`) producing virtio-ready images with
  headless BCD (EMS/SAC serial console).

**vhost-user devices**
- `SwiftGuest.spec.filesystems[]`: virtiofs shared filesystems (hostPath
  or PVC source, readOnly enforcement); swiftletd spawns virtiofsd
  (`--sandbox none`, no added capabilities) — full datapath
  cluster-validated.
- `GuestInterface.type: vhost-user` (+ `socket`, `mac`): virtio-net via an
  operator-provided DPDK/OVS backend.
- `SwiftGuest.spec.vhostUserDevices[]`: vhost-user-blk (SPDK-style) and
  generic vhost-user devices (`--generic-vhost-user`).
- Migration gate: guests with node-local virtio backends are offline-only
  (mirrors VFIO; auto resolves to offline).

**Networking**
- Multi-NIC: secondary interfaces via Multus NADs, SR-IOV VFIO NIC
  passthrough, mixed bridge+sriov guests.
- Multi-node L2 foundation: `GuestInterface.primary` lets the primary NIC
  ride a multi-node NAD (IP-preserving migration); corrected
  IP-preservation gate keyed on the primary interface; primary-on-NAD
  launcher runtime (EXPERIMENTAL — datapath pending validation on a
  multi-node L2 cluster); design + operator docs
  (`docs/design/network-architecture-requirements.md`,
  `docs/networking/multi-node-l2.md`).
- Networking operations guide, OVN-Kubernetes integration guide, ESXi/
  Proxmox concept mapping.

**Storage**
- `spec.storage` on SwiftGuestClass/SwiftGuest: accessMode / volumeMode /
  storageClassName selection with per-field merge; RWX+Block is the
  live-migration-capable combination (RWX+Filesystem rejected at
  admission); full Block-mode runtime path (volumeDevices end to end);
  `cloneStrategy: snapshot` works across volume modes
  (allow-volume-mode-change on the clone seed).

**GPU**
- SwiftGPU Phases 1–3: SwiftGPUProfile/SwiftGPUNode CRDs, allocation
  controller (NUMA-aware, FM partitions, finalizer dealloc), QEMU path
  (Q35/OVMF/QMP) for HGX tiers, GPU discovery DaemonSet (multi-vendor,
  60s cycle), Tier 1 PCIe passthrough validated on hardware (GTX 1080),
  IOMMU-group peer auto-binding, `vfioReady` + capacity pre-flights.

**Fleet & lifecycle**
- SwiftGuestPool: ReplicaSet-style fleets with rolling updates
  (maxUnavailable/maxSurge), topology spread, PVC-per-replica, scale
  subresource, node pre-assignment for snapshot clones.
- `dataDiskRef`/`dataDiskRefs` secondary disks (/dev/vdb) on all boot paths.
- Per-class vCPU core-scheduling (`SwiftGuestClass.spec.coreScheduling:
  off|vm|vcpu`) — SMT side-channel mitigation without disabling SMT.

**Operability & docs**
- GitOps: FluxCD reference repository (`examples/gitops-flux/`, three-layer
  model) + `docs/gitops/` operator docs.
- `swiftctl` grew `ssh`, `describe`, `logs`, robust pod resolution across
  migrations, and the command groups above.
- Controller-driven per-namespace swiftletd RBAC (no manual RoleBinding);
  `make deploy-with-webhook` / `deploy-with-webhook-and-mtls` targets;
  e2e suites wired into CI on path-touch triggers; THREAT-MODEL.md.

### Changed
- **Cloud Hypervisor v51.1 → v52.0** (platform-wide; Linux regression
  passed): fixes Windows viostor `0xD1` bugcheck; guests now **reset in
  place on reboot** (the launcher pod and CH survive — reboots no longer
  churn pods or trigger runPolicy); `CLOUDHV.fd` firmware unchanged
  (`ch-13b4963ec4`). CLOUDHV.fd replaced rust-hypervisor-firmware
  earlier in the cycle (all modern distros bootable; Ubuntu Noble is the
  primary guest OS).
- Guest RAM is now mapped `shared=on` (memfd MAP_SHARED): halves the
  launcher's guest-memory footprint and fixes memory-snapshot OOMs; the
  standard backing for snapshot/migration-capable guests.
- SwiftGuestClass default memory raised to 4Gi; launcher memory limits
  include a 512MiB overhead allowance.
- Root-disk import pipeline: qemu-img resize + sgdisk -e (GPT fix);
  cloud-init growpart expands on first boot.
- `status.observedPauseWindow` renamed to `status.observedTransferDuration`
  (it measures the full transfer RPC, not the vCPU pause).

### Fixed
- **Multi-NIC was silently broken end to end** (three stacked latent bugs):
  the network-init container had no runtime-intent mount (its multi-NIC
  path was unreachable), the launcher image lacked python3 (the intent
  parser), and vhost-user NICs tripped the NIC loop. All fixed; the
  long-flaky multi-nic smoke scenario now passes (smoke suite 5/5).
- **Tier A restore data loss** (silent fresh-boot instead of restored
  disk): `EnsureRootDiskClone` ordering fixed; regression-tested.
- Migration terminal-state handling: per-operation webhook discipline
  (finalizer traps, reconcile storms), chain-migration source-pod
  identity (`status.sourcePodRef`), offline-after-live pod-name trap,
  false-success on destination boot failure, downtime metrics anchored
  on real cutover timestamps.
- vswiftimage webhook: finalizer-removal trap on deletion AND
  pointer-identity spec comparison (metadata-only edits on Ready images
  were falsely rejected) — both fixed with content equality.
- swiftletd: lease poller survives transient RBAC/API failures; stale
  socket cleanup before CH spawn; receiver-mode GuestRunning reporting.
- S3 snapshot upload resume verifies sha256 (not just size); upload Job
  runs with the permissions the root-owned capture artifacts require.
- GPU walkthrough fixes: allocation re-stamp race, premature
  scheduling-atomicity check, reservation leak on guest-delete-mid-migration.
- `swiftctl debug` /proc scan anchors on argv[0] (no self-match);
  numerous gpu-init/container hardening fixes (sysfs shadowing, explicit
  interpreters, ASCII-only scripts).

### Known limitations (v0.3.0-rc.1)
- **Primary-on-NAD runtime is EXPERIMENTAL**: the launcher datapath is
  implemented but unvalidated (the dev cluster has no working multi-node
  L2); validate on an OVN-Kubernetes cluster before relying on
  IP-preserving migration.
- **vhost-user-net/-blk/generic datapaths are asset-gated**: CH wiring is
  cluster-validated, but line-rate operation needs operator-provided
  DPDK/SPDK backends (none on dev infra). virtiofs is fully validated.
- **SR-IOV NIC passthrough and Tier 2/3 HGX GPU support** are code-complete
  but hardware-unvalidated (no SR-IOV NICs / HGX systems available).
- **Cross-node GPU migration destination boot** is not hardware-validated
  (single GPU node); the release/reserve choreography is.
- **Windows in-cluster cloudbase-init provisioning** is untested (no
  Windows license on the dev cluster); every other Windows layer is
  validated.
- All API groups remain **v1alpha1**.

---

## [v0.1.0] — SwiftKernel + Networking (March 2026)

### Added

- SwiftImage import: HTTP source, qcow2-to-raw conversion, GRUB serial console patching
- SwiftGuest lifecycle: launcher pod creation, VM boot, status reporting via pod annotations
- Networking: tap+bridge+dnsmasq, guest IP discovery, status.network.primaryIP
- swiftctl CLI: console, start, stop, restart, debug, ssh, describe, logs
- SwiftSeedProfile: NoCloud cloud-init for user-data, SSH keys, network config
- RunPolicy: Running, Stopped, RestartOnFailure, Always with exponential backoff
- Observability: Prometheus metrics (boot time, running count, failure count, import time)
- SwiftKernel: per-node OCI artifact pull, kernel boot path (bzImage + initramfs)
- faas-minimal kernel profile: Linux 6.6.44 + BusyBox musl via buildroot
- SwiftKernel networking: DHCP IP via virtio-net on kernel boot guests
- Smoke test: end-to-end boot verification

### Known Issues

See the per-release notes below and the design docs under `docs/design/` for
known issues and their resolutions.
