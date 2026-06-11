# KubeSwift architecture — visual reference

Diagrams of KubeSwift's components, their interactions, and the data paths.
All diagrams are Mermaid and render directly on GitHub. Companion prose:
[architecture.md](../architecture.md), [control-plane.md](control-plane.md),
[node-runtime.md](node-runtime.md), [lifecycle.md](lifecycle.md).

Contents:

1. [System overview](#1-system-overview)
2. [CRD relationship map](#2-crd-relationship-map)
3. [Launcher pod anatomy & boot data path](#3-launcher-pod-anatomy--boot-data-path)
4. [Status reporting path](#4-status-reporting-path)
5. [Live migration sequence](#5-live-migration-sequence)
6. [Snapshot, restore & fast clones](#6-snapshot-restore--fast-clones)

---

## 1. System overview

Every box is a real deployable or a Kubernetes object. One controller-manager
hosts all reconcilers and (when `webhook.enabled`) all admission webhooks; the
only other long-running KubeSwift components are the per-guest **launcher pods**
and the opt-in **gpu-discovery DaemonSet**. VMs are pods: each SwiftGuest gets
one launcher pod whose `launcher` container runs `swiftletd`, which spawns the
hypervisor in-pod.

```mermaid
flowchart TB
    subgraph clients["Operator / User"]
        kubectl["kubectl / helm / GitOps"]
        swiftctl["swiftctl<br/>(start stop console ssh<br/>migrate snapshot restore)"]
    end

    subgraph apiserver["Kubernetes API server"]
        crds["KubeSwift CRDs<br/>swift / image / kernel / seed /<br/>gpu / snapshot / migration<br/>.kubeswift.io"]
        webhooks["Admission webhooks (opt-in)<br/>7 CRD validators + veviction<br/>(pods/eviction, drain gate)"]
    end

    subgraph cm["controller-manager (kubeswift-system)"]
        direction TB
        rec["Reconcilers:<br/>SwiftGuest · SwiftImage · SwiftKernel<br/>SwiftGuestPool · SwiftGPU · SwiftDrain<br/>SwiftSnapshot · SwiftRestore · SwiftSnapshotSchedule<br/>SwiftMigration · MigrationCert"]
        metrics["/metrics :8080<br/>feature metrics + CR-state collector"]
    end

    subgraph node["Worker node"]
        subgraph pod["Launcher pod (one per SwiftGuest)"]
            inits["init: clone-grow-init? → gpu-init? →<br/>snapshot-stager? → network-init?"]
            swiftletd["launcher: swiftletd (Rust)"]
            stunnel["sidecar: migration-stunnel?<br/>(mTLS, cross-pod :6789)"]
            hv["cloud-hypervisor (default)<br/>or qemu (HGX GPU tiers)"]
            vm(("Guest VM"))
        end
        gpudisc["gpu-discovery DaemonSet<br/>(nodes labeled kubeswift.io/gpu-node)"]
        kernelpull["SwiftKernel pull Jobs<br/>(ORAS → /var/lib/kubeswift/kernels)"]
    end

    obs["Prometheus / Grafana<br/>(Helm monitoring.* gate:<br/>ServiceMonitor, 6 dashboards, alerts)"]

    kubectl --> apiserver
    swiftctl --> apiserver
    apiserver <--> cm
    rec -- "creates per guest:<br/>intent CM · seed CM ·<br/>root-disk PVC · launcher pod ·<br/>PDB (maxUnavailable 0)" --> pod
    inits --> swiftletd
    swiftletd -- spawns --> hv
    hv --> vm
    swiftletd -. "status via pod annotations" .-> apiserver
    gpudisc -- "SwiftGPUNode.status<br/>(inventory, NUMA, vfioReady, FM)" --> apiserver
    rec --> kernelpull
    obs -- scrapes --> metrics
```

Key properties this encodes:

- **The API server is the only bus.** Controllers and swiftletd never talk to
  each other directly — coordination is CRD status, ConfigMaps and pod
  annotations (the one cross-pod TCP exception is live migration's CH-to-CH
  transfer, §5).
- **RestartPolicy of launcher pods is always `Never`**; the SwiftGuest
  controller owns restarts via `runPolicy`.
- The `?` init containers are conditional: `gpu-init` only for GPU guests,
  `snapshot-stager`/`clone-grow-init` only for restore/clone boots,
  `network-init` when the guest has networking, the stunnel sidecar only when
  `migration.mtls.enabled` and the guest is migration-eligible.

---

## 2. CRD relationship map

What references what. `SwiftGuest` is the hub; everything else either feeds it
(images, kernels, classes, seeds, GPU profiles) or operates on it (snapshots,
restores, migrations, pools, schedules).

```mermaid
flowchart LR
    subgraph fleet["Fleet"]
        POOL["SwiftGuestPool<br/>replicas · spreadPolicy ·<br/>rolling updates · PVC per replica"]
    end

    SG["<b>SwiftGuest</b><br/>runPolicy · osType · storage ·<br/>interfaces · filesystems ·<br/>vhostUserDevices · migration policy"]

    subgraph boot["Boot source (exactly one)"]
        IMG["SwiftImage<br/>http qcow2 → raw PVC<br/>cloneStrategy: copy | snapshot"]
        KRN["SwiftKernel<br/>OCI artifact (ORAS) →<br/>bzImage + initramfs per node"]
        SNAPREF["cloneFromSnapshot<br/>(memory-state clone)"]
    end

    subgraph shape["VM shape"]
        CLS["SwiftGuestClass<br/>cpu · memory · rootDisk ·<br/>storage (accessMode/volumeMode) ·<br/>coreScheduling"]
        SEED["SwiftSeedProfile<br/>NoCloud cloud-init /<br/>cloudbase-init (Windows)"]
    end

    subgraph gpu["GPU (at most one backend)"]
        GPUP["SwiftGPUProfile (native)<br/>count · model · tier ·<br/>partitionMode · NUMA · hugepages"]
        GPURC["gpuResourceClaim (DRA, opt-in)<br/>ResourceClaim/Template + tier"]
        GPUN["SwiftGPUNode (cluster-scoped)<br/>inventory by gpu-discovery ·<br/>allocations by SwiftGPU controller"]
    end

    subgraph dataprot["Data protection & mobility"]
        SNAP["SwiftSnapshot<br/>backend: csi-volume-snapshot |<br/>local | s3 · includeMemory"]
        REST["SwiftRestore<br/>in-place or clone ·<br/>identity regeneration"]
        SCHED["SwiftSnapshotSchedule<br/>cron + keep-N retention"]
        MIG["SwiftMigration<br/>mode: auto | live | offline ·<br/>target node · ttl"]
    end

    POOL -- "template mints" --> SG
    SG -- imageRef --> IMG
    SG -- kernelRef --> KRN
    SG -- cloneFromSnapshot --> SNAPREF
    SNAPREF -- snapshotRef --> SNAP
    SG -- guestClassRef --> CLS
    SG -- seedProfileRef --> SEED
    SG -- gpuProfileRef --> GPUP
    SG -- gpuResourceClaim --> GPURC
    GPUP -. "allocated on" .-> GPUN
    SNAP -- guestRef --> SG
    REST -- snapshotRef --> SNAP
    SCHED -- "template mints" --> SNAP
    MIG -- guestRef --> SG
```

Constraint highlights (enforced by the SwiftGuest webhook): `imageRef` /
`kernelRef` / `cloneFromSnapshot` are mutually exclusive boot sources;
`gpuProfileRef` XOR `gpuResourceClaim`; GPU combines with `imageRef` but never
`kernelRef`; `osType: windows` is disk-boot only and GPU-less in v1;
RWX+Filesystem storage is rejected (RWX+Block is the live-migration-capable
combination).

---

## 3. Launcher pod anatomy & boot data path

The disk-boot data path end to end — from a qcow2 URL to a running VM with an
IP — including where every artifact lives and which component produces it.

```mermaid
flowchart TB
    subgraph import["SwiftImage import (once per image)"]
        url["http qcow2 / raw image"]
        ijob["import Job<br/>download → qemu-img convert -O raw →<br/>GRUB serial-console patch (Linux only;<br/>skipped for osType windows) →<br/>qemu-img resize + sgdisk -e"]
        ipvc[("image PVC<br/>prepared raw image")]
        cseed[("clone-seed VolumeSnapshot<br/>(cloneStrategy: snapshot only)")]
        url --> ijob --> ipvc -. snapshot strategy .-> cseed
    end

    subgraph perguest["Per-guest provisioning (SwiftGuest controller)"]
        clone["root-disk clone<br/>copy: Copy Job (cp / qemu-img to<br/>volumeDevice for Block) ·<br/>snapshot: CSI clone of clone-seed"]
        gpvc[("per-guest root PVC<br/>RWO+Filesystem (default) or<br/>RWX+Block (live-migratable)")]
        seedcm["&lt;guest&gt;-seed ConfigMap<br/>(NoCloud user-data/meta-data)"]
        intentcm["&lt;guest&gt;-runtime-intent ConfigMap<br/>(JSON: cpu, memory, disks, network,<br/>gpu, lifecycle, restore...)"]
    end

    subgraph lpod["Launcher pod (on the scheduled node)"]
        direction TB
        ni["network-init (init):<br/>create br0 192.168.99.1/24 + tap0,<br/>eth0 NOT enslaved (Bug 1)"]
        gi["gpu-init (init, GPU only):<br/>two-pass vfio-pci bind of the<br/>IOMMU group + FM partition activate"]
        sl["swiftletd:<br/>read intent → build seed.iso (swift-seed) →<br/>spawn hypervisor → start dnsmasq →<br/>poll DHCP lease → report status"]
        subgraph hyp["Hypervisor"]
            ch["cloud-hypervisor:<br/>--kernel CLOUDHV.fd --disk raw+seed.iso<br/>(disk) · --kernel bzImage (kernel) ·<br/>--restore file://... (clone/restore) ·<br/>--cpus kvm_hyperv=on (Windows)"]
            qemu["qemu (HGX GPU tiers):<br/>OVMF + pcie-root-port per GPU +<br/>vfio-pci + 1Gi hugepages, QMP control"]
        end
        guest(("Guest VM<br/>eth0 ← tap0/br0 DHCP"))
    end

    ipvc --> clone --> gpvc
    gpvc -- "mounted (file) or<br/>volumeDevice /dev/kubeswift-root (Block)" --> lpod
    seedcm --> sl
    intentcm --> sl
    ni --> sl
    gi --> sl
    sl --> hyp --> guest
    guest -- "DHCP lease (dnsmasq)" --> sl
```

Notes:

- **Runtime disks are always raw**; qcow2 is input-only. `CLOUDHV.fd` loads via
  `--kernel`, never `--firmware` (that flag is for OVMF on the QEMU path).
- The guest network is **node-local by default**: each launcher pod has its own
  `br0` (192.168.99.0/24) + dnsmasq, so guest IPs are per-pod and change across
  nodes — the reason cross-node migration needs `allowIPChange` or a multi-node
  NAD on the primary interface.
- Kernel-boot guests skip the whole import/clone column — no PVC at all, which
  is why they are the lightest guests to live-migrate.
- swiftletd talks to CH over its HTTP API on a Unix socket (`ch.sock`) and to
  QEMU over QMP; the serial console is a Unix socket consumed by
  `swiftctl console`.

---

## 4. Status reporting path

swiftletd has no API server write access to SwiftGuest status. It reports via
**pod annotations**, which the SwiftGuest controller maps to status on
reconcile. The one exception is the `GuestRunning` condition, patched directly
via kube-rs (DynamicObject) so phase transitions are immediate.

```mermaid
sequenceDiagram
    participant SL as swiftletd (launcher pod)
    participant POD as Pod object (annotations)
    participant API as API server
    participant CTRL as SwiftGuest controller
    participant SG as SwiftGuest.status

    Note over SL: VM spawned
    SL->>POD: kubeswift.io/guest-runtime-pid, guest-serial-socket, guest-hypervisor
    SL->>SG: GuestRunning=True (kube-rs DynamicObject — the one exception)
    loop lease poller (2s tick, retry until success)
        SL->>POD: kubeswift.io/guest-ip, guest-interfaces (from dnsmasq lease)
    end
    POD-->>API: annotation patches
    API-->>CTRL: watch event (pod changed)
    CTRL->>SG: map annotations → status.runtime.pid, console.serialSocket,<br/>runtime.hypervisor, network.primaryIP, network.interfaces[]

    Note over POD,CTRL: The same annotation surface carries ACTIONS downward:
    CTRL->>POD: kubeswift.io/snapshot-action / migration-action (+ id)
    SL->>POD: action-id mirror + result (status-id-paired write)
    CTRL->>SG: observe completion on next reconcile
```

This annotation surface is deliberate: swiftletd needs only a namespaced Role
on pods (auto-bound per namespace by the controller), and every interaction is
observable with `kubectl get pod -o yaml`.

---

## 5. Live migration sequence

`mode: live` for an eligible guest (no VFIO, no virtiofs/vhost-user, RWX+Block
or kernel-boot). The controller orchestrates two swiftletds purely through the
annotation surface; the only cross-pod TCP is the CH-to-CH state transfer, which
flows through mTLS stunnel sidecars (per-node cert-manager identities, SAN
pinning). Measured baseline: **~2–3 s downtime**, ~39 s transfer for a 4 Gi
guest.

```mermaid
sequenceDiagram
    autonumber
    participant OP as operator / drain controller
    participant MC as SwiftMigration controller
    participant SRC as src launcher pod<br/>(swiftletd + stunnel client)
    participant DST as dst launcher pod<br/>(swiftletd + stunnel server)
    participant SG as SwiftGuest

    OP->>MC: SwiftMigration (guestRef, target, mode: live)
    Note over MC: Validating — eligibility, target capacity,<br/>mTLS identity Secrets, lock SourcePodUID/Name
    MC->>DST: create dst pod on target node<br/>(frozen lifecycle:run intent, server-role stunnel,<br/>receive-mode swiftletd)
    Note over MC: Preparing — wait dst Ready (60s budget)
    MC->>DST: annotation: migration-action receive<br/>(listen tcp:127.0.0.1:6790 ← stunnel :6789)
    MC->>SRC: annotation: migration-action send<br/>(target tcp:127.0.0.1:6790 → stunnel client)
    SRC-->>DST: CH vm.send-migration over mTLS :6789<br/>pre-copy iterations (5-iter cap) + final stop-and-copy
    SRC->>SRC: progress-estimate annotations (13→…→92%)
    Note over MC: StopAndCopy — observe src "complete" via informer
    MC->>SG: cutover step 1: status.podRef → dst pod
    MC->>SRC: cutover step 2: delete src pod (downtime clock starts)
    DST->>SG: GuestRunning=True (vCPUs resumed on dst)
    Note over MC: Resuming — gate Completed on REAL dst state<br/>(launcher Ready, not stale conditions)
    MC->>SG: Completed · observedDowntime ≈ 2–3 s ·<br/>observedTransferDuration ≈ 39 s
    Note over MC: ttl-based retention GC's the record later
```

Offline mode (the only mode for VFIO/GPU and virtiofs/vhost-user guests) is
simpler: stop source → wait for pod + VolumeAttachment to clear → recreate the
guest pinned to the target reusing the same PVC (~25–70 s downtime depending on
storage). GPU guests add reserve-target-GPUs-before-stop via
`ReserveOnNode`/`ReleaseFromNode`.

---

## 6. Snapshot, restore & fast clones

Three snapshot backends share one CRD; restores and memory-clones reuse the
launcher's restore-receive path (`CH --restore`).

```mermaid
flowchart TB
    SG["Running SwiftGuest"]

    subgraph capture["SwiftSnapshot capture"]
        A["Tier A — csi-volume-snapshot<br/>disk-only · CSI VolumeSnapshot<br/>(e.g. Longhorn)"]
        B["Tier B — local<br/>disk+memory · CH pause/snapshot/resume<br/>→ node hostPath<br/>(sub-second pause window)"]
        C["Tier C — s3<br/>Tier B capture + node-pinned upload Job<br/>→ checksummed manifest in object storage"]
    end

    subgraph consume["Consumers"]
        REST["SwiftRestore<br/>in-place (overwrite) or clone ·<br/>Tier C: node-pinned download Job first"]
        CLONE["SwiftGuest.cloneFromSnapshot<br/>fresh root disk + CH --restore of RAM ·<br/>per-clone hypervisor MAC ·<br/>identity regen on first reboot"]
        SCHED["SwiftSnapshotSchedule<br/>cron mints SwiftSnapshots ·<br/>keep-N prune (reference-aware) · ttl"]
        POOLC["SwiftGuestPool template<br/>N memory-clones, one per node<br/>(Tier C, round-robin targetNode)"]
    end

    SG --> A & B & C
    A --> REST
    B --> REST
    C --> REST
    B --> CLONE
    C --> CLONE
    SCHED -. mints .-> capture
    CLONE -.-> POOLC
```

The honest performance note (measured on Longhorn full-copy): a memory-clone
still provisions a **fresh root disk**, so on full-copy storage it can be
*slower* than a cold boot; the win comes on CoW storage (near-instant disk
clone → seconds to a running, pre-warmed VM) or when the captured state is
expensive to recreate. Details and numbers:
[../snapshots/fast-vms.md](../snapshots/fast-vms.md).
