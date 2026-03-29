# KubeSwift Project Context
> This document is the canonical context anchor for AI-assisted KubeSwift development.
> It should be read at the start of every new session before any work begins.
> Last updated: March 29, 2026 — SwiftKernel end-to-end verified, kernel boot path complete

---

## What is KubeSwift

KubeSwift is a Kubernetes-native virtual machine runtime built on Cloud Hypervisor.
It allows Kubernetes users to run virtual machines as first-class Kubernetes workloads,
using custom resources and controllers.

KubeSwift is **not** a container sandbox (not Kata Containers).
It is a VM platform where virtual machines are first-class Kubernetes workloads.

It is similar in spirit to KubeVirt but with a much simpler architecture:
- Runtime: Cloud Hypervisor (not QEMU)
- Firmware: rust-hypervisor-firmware v0.5.0 (disk boot) or direct bzImage (kernel boot)
- Distribution: OCI-native (Helm chart + container images)
- Goal: minimal architecture, strong operability, fast iteration

Repository: `https://github.com/projectbeskar/kubeswift` (private)
Images: `ghcr.io/projectbeskar/kubeswift/` (public packages)

---

## Current State (v0.1.0 + SwiftKernel)

### What works end-to-end
- SwiftImage import: downloads qcow2, converts to raw, patches GRUB for serial console
- SwiftGuest lifecycle: creates launcher pod, boots VM, reports status
- Networking: tap+bridge+dnsmasq DHCP, guest gets IP, IP propagated to status
- `swiftctl console`: connects to serial socket, interactive console works
- `swiftctl start/stop/restart/debug`: implemented and working
- `swiftctl ssh <guest>`: SSH into guest via launcher pod with --user and --identity flags
- `swiftctl describe <guest>`: rich human-readable status
- `swiftctl logs <guest>`: tail swiftletd launcher logs
- Rich guest status: runtime.pid, runtime.hypervisor, console.serialSocket, network.interfaces[]
- Graceful stop via SIGTERM + 30s fallback to pod delete
- RestartPolicy=Never on launcher pod
- Controller stopped guard — no pod recreation when runPolicy=Stopped
- Image pipeline: sourceFormat, preparedFormat, size measurement via post-import job
- Smoke test: passes end-to-end (`make smoke-test`)
- Observability: Prometheus metrics, kubeswift_guest_running_total, kubeswift_vm_boot_seconds,
  kubeswift_vm_failures_total, kubeswift_image_import_seconds
- **SwiftKernel: per-node OCI artifact pull, kernel boot path, verified on CH v51.1**

### Known working configuration
- Guest OS (disk boot): **Ubuntu Focal (20.04)** cloud image — Noble (24.04) is incompatible
  with rust-hypervisor-firmware due to EFI protocol gaps (TPM, MOK, writable NVRAM)
- Guest OS (kernel boot): **faas-minimal** — Linux 6.6.44 + BusyBox musl, built with buildroot
- Firmware (disk boot): rust-hypervisor-firmware v0.5.0, loaded via `--kernel` flag (NOT `--firmware`)
- Firmware (kernel boot): direct bzImage via `--kernel`, initramfs via `--initramfs`
- Cloud Hypervisor: v51.1
- Seed format: NoCloud flat layout (meta-data, user-data, network-config at ISO root)
- Seed ISO: genisoimage with `-rock -joliet -volid cidata` flags
- DHCP range: 10.244.125.10–20 on br0 (10.244.125.1)
- ORAS CLI: v1.3.1 (used in SwiftKernel pull jobs)

### Deployed images (latest)
- `ghcr.io/projectbeskar/kubeswift/controller-manager:sha-<latest>`
- `ghcr.io/projectbeskar/kubeswift/swiftletd:sha-<latest>`
- `ghcr.io/projectbeskar/kubeswift/kernels/faas:6.6.0` (OCI artifact, not a container image)

---

## Architecture

### High-Level
```
User
 │
 │ kubectl / helm / swiftctl
 │
 ▼
Kubernetes API Server
 │  CRDs
 ▼
KubeSwift Controllers (Go, controller-runtime)
 │  create launcher pod
 ▼
SwiftGuest Pod
 ├─ init container: network-init (bridge/tap/dnsmasq setup) — disk boot only
 └─ launcher container: swiftletd (Rust)
        │
        ▼
     Cloud Hypervisor v51.1
        │  disk boot:   --kernel hypervisor-fw --disk image.raw --disk seed.iso
        │  kernel boot: --kernel bzImage --initramfs rootfs.cpio.gz --cmdline "..."
        ▼
      Guest VM
```

### CRDs

**SwiftGuest** — represents a running VM
```yaml
spec:
  imageRef:       # SwiftImage to boot from (optional, mutually exclusive with kernelRef)
  kernelRef:      # SwiftKernel to boot from (optional, mutually exclusive with imageRef)
  kernelCmdline:  # Per-guest kernel cmdline override (kernel boot only)
  guestClassRef:  # SwiftGuestClass for resource defaults
  seedProfileRef: # SwiftSeedProfile for cloud-init (disk boot only)
  runPolicy:      # Running | Stopped | RestartOnFailure | Always
status:
  phase:          # Pending | Scheduling | Running | Failed | Stopped
  conditions:     # Resolved | PodScheduled | GuestRunning
  nodeName:
  podRef:
  runtime:
    pid:          # Cloud Hypervisor process PID
    hypervisor:   # "cloud-hypervisor"
  console:
    serialSocket: # path to serial.sock
  network:
    primaryIP:    # discovered from dnsmasq DHCP lease (disk boot only)
    interfaces:   # list of {name, ip}
```

**SwiftImage** — represents a VM disk image
```yaml
spec:
  source:
    http:
      url: https://cloud-images.ubuntu.com/focal/current/focal-server-cloudimg-amd64.img
  format: qcow2   # converted to raw during import
status:
  phase: Ready | Importing | Validating | Preparing | Failed
  sourceFormat:   # original input format (e.g. qcow2)
  preparedFormat: # runtime format (always raw)
  preparedArtifact:
    pvcRef:       # PVC containing image.raw
    format: raw
    size:         # actual measured size of image.raw
```

**SwiftKernel** — represents a MicroVM kernel + initramfs OCI artifact
```yaml
spec:
  ociRef:
    image: ghcr.io/projectbeskar/kubeswift/kernels/faas:6.6.0
    pullSecret: ""      # optional, for private registries
  kernelCmdline: "console=ttyS0 root=/dev/ram0 rdinit=/init"  # default cmdline
  profile: faas-minimal # informational label
status:
  phase: Pending | Pulling | Ready | Failed
  conditions:     # Ready | Failed | NoKernelNodes
  nodeStatuses:   # per-node pull progress
  - nodeName: miles
    phase: Ready
```

**SwiftSeedProfile** — cloud-init / NoCloud configuration
```yaml
spec:
  datasource: NoCloud
  userData: |    # cloud-config YAML
  metaData: |
  networkData: |
```

**SwiftGuestClass** — default resource profile
```yaml
spec:
  cpu: 2
  memory: 2048Mi
  rootDisk:
    size: 40Gi
    format: raw
```

### Repository Structure
```
api/
  image/v1alpha1/         SwiftImage types
  kernel/v1alpha1/        SwiftKernel types
  seed/v1alpha1/          SwiftSeedProfile types
  swift/v1alpha1/         SwiftGuest, SwiftGuestClass types
  shared/                 common types

cmd/
  swiftctl/               CLI
  controller-manager/     main entry point
  webhook-server/         webhook entry point

internal/
  controller/swiftguest/  SwiftGuest controller
  controller/swiftimage/  SwiftImage controller
  controller/swiftkernel/ SwiftKernel controller (per-node pull)
  runtimeintent/          VM launch spec builder (disk + kernel boot paths)
  resolved/               Resolution and merge logic
  seed/                   cloud-init ConfigMap builder
  scheme/                 scheme registration (all 4 API groups)

rust/
  swiftletd/              VM launcher (disk boot + kernel boot paths)
  swift-runtime/          RuntimeDir management
  swift-ch-client/        Cloud Hypervisor spawn + API client
  swift-seed/             NoCloud ISO builder

build/
  kernels/
    faas-minimal/         buildroot external tree for faas-minimal kernel profile
      configs/            defconfig, linux config, busybox config
      rootfs-overlay/     /init script
      Makefile            build + verify-boot targets
      verify-boot.sh      boot verification script

images/
  swiftletd/Containerfile
  controller-manager/
  webhook-server/

config/
  crd/bases/              Generated CRD YAML
  samples/                Sample manifests
charts/kubeswift/         Helm chart (includes all 4 CRDs)
test/smoke/               End-to-end smoke test
```

### Networking Model (disk boot)
```
Guest VM
   │ virtio-net
   │
  tap0
   │
  br0 (10.244.125.1/24)
   │
  pod network (eth0, NOT bridged)

dnsmasq: DHCP range 10.244.125.10–20, router 10.244.125.1
iptables: MASQUERADE on eth0 for guest outbound traffic
```

Key facts:
- `eth0` is **not** enslaved to `br0` — this was Bug 1 and broke pod networking
- `br0` has its own IP (`10.244.125.1`) as the default gateway for guests
- swiftletd polls dnsmasq lease file to discover guest IP
- Guest IP written to pod annotation `kubeswift.io/guest-ip`
- Controller reads annotation and writes to `SwiftGuest.status.network.primaryIP`
- Controller requeues every 5s while Running but no IP yet (cache staleness fix)
- Kernel boot (faas-minimal): no networking — `network: false` in runtime intent

### SwiftKernel Architecture
```
SwiftKernel created
  │
  ▼
Controller lists nodes with label kubeswift.io/kernel-node=true
  │
  ├─ No labeled nodes → phase=Pending, condition=NoKernelNodes, log warning
  │
  └─ For each labeled node:
       Create pull Job: swiftkernel-pull-<name>-<nodename>
       Job nodeSelector: kubeswift.io/kernel-node=true + kubernetes.io/hostname=<node>
       Job runs: oras pull <image> → /var/lib/kubeswift/kernels/<ns>-<name>/
       │
       ▼
       status.nodeStatuses[nodeName].phase = Ready
  │
  ▼
All nodes Ready → status.phase = Ready
```

Key facts:
- Node opt-in label: `kubeswift.io/kernel-node=true`
- Artifact path on node: `/var/lib/kubeswift/kernels/<namespace>-<name>/bzImage`
  and `/var/lib/kubeswift/kernels/<namespace>-<name>/rootfs.cpio.gz`
- LocalPath is a deterministic function, not stored in status
- Controller watches Node label changes and requeues all SwiftKernels
- ORAS image: `ghcr.io/oras-project/oras:v1.3.1`
- OCI artifact media types:
  - kernel: `application/vnd.kubeswift.kernel.binary`
  - initramfs: `application/vnd.kubeswift.initramfs.binary`
  - artifact type: `application/vnd.kubeswift.kernel.v1`

### Kernel Boot Path (SwiftGuest with kernelRef)
```
SwiftGuest (kernelRef=faas-minimal)
  │
  ▼
Controller resolves SwiftKernel → localPath
  │
  ▼
RuntimeIntent:
  kernelBoot:
    kernelPath:    /var/lib/kubeswift/kernels/default-faas-minimal/bzImage
    initramfsPath: /var/lib/kubeswift/kernels/default-faas-minimal/rootfs.cpio.gz
    cmdline:       console=ttyS0 root=/dev/ram0 rdinit=/init
  network: false
  rootDisk: {path: "", format: ""}
  │
  ▼
Pod:
  nodeSelector: kubeswift.io/kernel-node=true
  volumes: run, runtime-intent, dev-kvm, kernel-artifacts (hostPath)
  NO: root-disk PVC, seed volume, network-init init container
  │
  ▼
swiftletd:
  cloud-hypervisor \
    --kernel bzImage \
    --initramfs rootfs.cpio.gz \
    --cmdline "console=ttyS0 root=/dev/ram0 rdinit=/init" \
    --serial socket=serial.sock \
    --console off
```

### Image Import Pipeline
```
SwiftImage created
  │
  ▼
Import Job (ubuntu:22.04, privileged)
  │  curl download
  │  qemu-img convert qcow2 → raw (if needed)
  │  GPT partition detection via od (NOT fdisk — fails on GPT)
  │  mount each partition, patch grub.cfg for serial console
  │  stat -c %s image.raw > image.raw.size
  ▼
Measure Job (ubuntu:22.04)
  │  cat /data/image.raw.size → parsed as int64
  ▼
image.raw stored in PVC
status.phase = Ready
```

### Cloud Hypervisor Invocation

**Disk boot:**
```
cloud-hypervisor \
  --kernel /usr/share/kubeswift-firmware/hypervisor-fw \
  --api-socket path=<runtime-dir>/ch.sock \
  --memory size=2048M \
  --cpus boot=2 \
  --disk path=<image.raw> path=<seed.iso> \
  --serial socket=<runtime-dir>/serial.sock \
  --console off \
  --net tap=tap0
```

**Kernel boot:**
```
cloud-hypervisor \
  --kernel <localPath>/bzImage \
  --initramfs <localPath>/rootfs.cpio.gz \
  --cmdline "console=ttyS0 root=/dev/ram0 rdinit=/init" \
  --api-socket path=<runtime-dir>/ch.sock \
  --memory size=2048M \
  --cpus boot=2 \
  --serial socket=<runtime-dir>/serial.sock \
  --console off
```

Critical:
- `--kernel` with rust-hypervisor-firmware = PVH ELF binary (disk boot)
- `--kernel` with bzImage = direct Linux kernel (kernel boot)
- `--firmware` is for OVMF/EDK2 only — never use it
- `imageRef` and `kernelRef` are mutually exclusive on SwiftGuest

### Status Reporting Architecture

swiftletd reports status via pod annotations (not SwiftGuest status directly).
The controller reads annotations on reconcile and maps them to SwiftGuest status.

| Annotation | Set by | Maps to |
|---|---|---|
| `kubeswift.io/guest-ip` | lease.rs (DHCP discovery) | status.network.primaryIP |
| `kubeswift.io/guest-interfaces` | lease.rs | status.network.interfaces[] |
| `kubeswift.io/guest-runtime-pid` | report.rs (socket ready) | status.runtime.pid |
| `kubeswift.io/guest-serial-socket` | report.rs (socket ready) | status.console.serialSocket |

GuestRunning condition is patched directly to SwiftGuest status by swiftletd via
kube-rs DynamicObject patch (not via pod annotation).

---

## Bugs Fixed (Historical Reference)

| # | Component | Bug | Fix |
|---|-----------|-----|-----|
| 1 | network-init.sh | eth0 enslaved to br0, broke pod network | Remove eth0 from bridge, add NAT/masquerade |
| 2 | swift-seed | OpenStack ConfigDrive layout instead of NoCloud | Write flat files at ISO root |
| 3 | swiftletd | genisoimage missing -rock/-joliet/-volid flags | Add flags to genisoimage invocation |
| 4 | swift-runtime | genisoimage relative output path | Canonicalize output path |
| 5 | swiftimage/import.go | fdisk fails on GPT disks | Replace with od-based GPT LBA parser |
| 6 | swiftimage/import.go | GRUB patch skipped BOOT partition (ttyS0 already present) | Add unconditional terminal patch block |
| 7 | swiftimage/import.go | GRUB terminal patch order wrong (terminal_input before serial init) | Use awk rewrite for correct ordering |
| 8 | swift-ch-client/config.rs | --firmware used instead of --kernel | Revert to --kernel |
| 9 | config/samples/image.yaml | Ubuntu Noble incompatible with rust-hypervisor-firmware | Switch to Ubuntu Focal |
| 10 | swiftguest controller | No requeue when IP not yet discovered | Add RequeueAfter: 5s when Running + no IP |
| 11 | CRD schema | status.network missing from OpenAPI schema | Regenerate CRD, add to make deploy |
| 12 | smoke test | 2m network timeout too short | Increase to 5m, add --timeout-network flag |
| 13 | Makefile | make deploy didn't apply CRDs | Add kubectl apply -k config/crd + kubectl wait |
| 14 | seed profile | package_update: true caused slow first boot | Remove from sample seed profile |
| 15 | swiftletd/main.rs | on_socket_ready closure used nested tokio runtime | Reuse outer Arc<Runtime> via rt_clone |
| 16 | swiftletd/lease.rs | guest-interfaces annotation double-escaped JSON | Use serde_json::json! instead of format! |
| 17 | swiftletd/report.rs | report_guest_runtime patched SwiftGuest status directly | Patch pod annotations instead |
| 18 | swiftguest/pod.go | RestartPolicy not set, defaulted to Always | Set RestartPolicy: Never unconditionally |
| 19 | swiftguest/controller.go | Controller recreated pod when runPolicy=Stopped | Add stopped guard before pod create logic |
| 20 | rbac.yaml | controller-manager missing pods/log get permission | Add pods/log get rule to ClusterRole |
| 21 | swiftkernel/pull.go | --media-type flag not supported by oras v1.x | Remove flag, pull all layers |
| 22 | swiftkernel OCI artifact | Layer titles had full build path prefix | Repush from artifact directory for clean titles |
| 23 | swiftkernel CRD | nodeStatuses field missing from OpenAPI schema | Regenerate CRD after adding NodeKernelStatus type |
| 24 | rbac.yaml | controller-manager missing kernel.kubeswift.io permissions | Add SwiftKernel rules to ClusterRole |
| 25 | rbac.yaml | controller-manager missing nodes get/list/watch | Add nodes rule for SwiftKernel per-node scheduling |

---

## Deployment
```bash
make build-images push-images deploy
```

`make deploy` must:
1. Run `controller-gen` to regenerate CRD YAML from Go types
2. `kubectl apply -k config/crd` + wait for Established
3. Deploy controller-manager

**Never let the CRD schema drift from the Go types.** The API server silently drops unknown fields.

After CRD changes, always:
```bash
make generate
cp config/crd/bases/*.yaml charts/kubeswift/crds/
```

### Smoke Test
```bash
make smoke-test
# or with custom timeouts:
./test/smoke/boot-test.sh --timeout-image 15 --timeout-guest 5 --timeout-network 5
```

Success criteria:
- `GuestRunning=True`
- `status.network.primaryIP` populated

### SwiftKernel Quick Test
```bash
# Label a node
kubectl label node <nodename> kubeswift.io/kernel-node=true

# Create kernel artifact
kubectl apply -f config/samples/swiftkernel-faas.yaml
kubectl get swiftkernel faas-minimal -w  # wait for Ready

# Create guest using kernel boot
kubectl apply -f config/samples/swiftguest-faas.yaml
kubectl get swiftguest faas-test -w  # wait for Running
```

---

## Roadmap

### Completed (v0.1.0)
- VM boots end-to-end ✓
- Networking works ✓
- IP discovery and status reporting ✓
- swiftctl console/start/stop/restart/debug/ssh/describe/logs ✓
- Smoke test passes ✓
- Rich guest status ✓
- Graceful stop, RestartPolicy=Never, stopped guard ✓
- Image pipeline improvements ✓
- runPolicy: RestartOnFailure | Always with exponential backoff ✓
- Documentation ✓
- Observability: Prometheus metrics ✓

### Completed (SwiftKernel)
- faas-minimal buildroot profile: Linux 6.6.44 + BusyBox musl ✓
- Boot verified on Cloud Hypervisor v51.1 ✓
- OCI packaging: ghcr.io/projectbeskar/kubeswift/kernels/faas:6.6.0 ✓
- SwiftKernel CRD: Pending | Pulling | Ready | Failed ✓
- Per-node pull via kubeswift.io/kernel-node=true label ✓
- nodeStatuses per-node tracking ✓
- SwiftGuest kernelRef boot path ✓
- RuntimeIntent kernel boot fields ✓
- swiftletd kernel boot: --kernel --initramfs --cmdline ✓
- SwiftGuest phase=Running with kernel boot verified ✓

### Next Priorities (in order)

**1. Host runtime hardening**
- network-init needs: NET_ADMIN, NET_RAW (no SYS_ADMIN needed)
- launcher needs: NET_ADMIN, SYS_ADMIN (for KVM ioctls)
- Current: both containers run privileged: true — works but overprivileged
- Approach: harden network-init first, verify, then harden launcher
- Risk: breaking change if capabilities are wrong — test on dedicated cluster

**2. SwiftKernel networking**
- faas-minimal currently boots with network: false
- Add virtio-net support to faas-minimal kernel config
- Add tap+bridge setup for kernel boot pods (similar to disk boot)
- Update runtime intent network field for kernel boot path

**3. SwiftKernel additional profiles**
- gpu-workload profile
- vhost-user profile
- Build pipeline at build/kernels/<profile>/

**4. dataDiskRef on SwiftGuest**
- Kernel boot guests may need persistent storage
- New optional field: spec.dataDiskRef pointing to a SwiftImage
- Adds --disk to cloud-hypervisor invocation alongside --kernel/--initramfs

### Long-term (not yet prioritized)
- Live migration
- Snapshots / persistent disks
- SR-IOV / Multus networking
- GPU passthrough
- High availability controllers

---

## SwiftKernel Build Notes

### faas-minimal profile
- Location: `build/kernels/faas-minimal/`
- Buildroot version: 2024.02.6
- Linux version: 6.6.44
- Userspace: BusyBox 1.36.1, statically linked against musl
- Build: `cd build/kernels/faas-minimal && make setup && make build`
- Outputs: `output/images/bzImage` (4.5MB), `output/images/rootfs.cpio.gz` (497KB)
- Boot verified: cloud-hypervisor v51.1 on node miles

### OCI packaging
- Push from artifact directory (not repo root) for clean layer titles:
```bash
  cd build/kernels/faas-minimal/output/images
  oras push ghcr.io/projectbeskar/kubeswift/kernels/faas:6.6.0 \
    --artifact-type application/vnd.kubeswift.kernel.v1 \
    --annotation "org.opencontainers.image.description=KubeSwift faas-minimal kernel 6.6.44" \
    bzImage:application/vnd.kubeswift.kernel.binary \
    rootfs.cpio.gz:application/vnd.kubeswift.initramfs.binary
```
- Layer titles must be `bzImage` and `rootfs.cpio.gz` (no path prefix)

### Kernel requirements
- Base: Cloud Hypervisor reference config
- Required: CONFIG_VIRTIO=y, CONFIG_VIRTIO_PCI=y, CONFIG_VIRTIO_NET=y,
  CONFIG_VIRTIO_BLK=y, CONFIG_SERIAL_8250=y, CONFIG_SERIAL_8250_CONSOLE=y,
  CONFIG_TTY=y, CONFIG_BINFMT_ELF=y, CONFIG_TMPFS=y, CONFIG_PROC_FS=y,
  CONFIG_SYSFS=y, CONFIG_BLK_DEV_INITRD=y

---

## Design Principles

When contributing, always follow these:

1. **Minimalism** — avoid unnecessary complexity, deps, abstraction layers
2. **Cloud Hypervisor first** — all decisions must be compatible with CH v51+
3. **Raw disk at runtime** — qcow2 is input only; runtime always uses raw
4. **Kubernetes-native** — everything observable via kubectl; status fields must be accurate
5. **Strong operability** — operators must be able to discover IP, connect console, SSH, inspect status
6. **No silent failures** — status fields must reflect real system state; never drop errors silently
7. **Verified fixes only** — no speculative patches; diagnose with real cluster output first
8. **Distributed by design** — no single-node assumptions; per-node artifact management via labels

---

## AI Assistant Instructions

When helping develop KubeSwift:

- Read this document and the session transcript before starting any work
- Check `/mnt/transcripts/journal.txt` for previous session summaries
- Prefer minimal changes — one bug fix at a time, verified with real output
- Always ask for actual cluster output before suggesting fixes
- Never assume a fix worked without seeing logs confirming it
- When writing Cursor prompts: be explicit about what NOT to change
- CRD changes always require `make generate` + copy to charts/kubeswift/crds/ + redeploy
- The working guest OS (disk boot) is Ubuntu Focal — do not suggest Noble
- rust-hypervisor-firmware uses `--kernel`, not `--firmware`
- The GRUB terminal patch order is: serial init → terminal_input → terminal_output
- swiftletd reports status via pod annotations, not direct SwiftGuest status patches
- RestartPolicy on launcher pods is always Never — controller owns VM lifecycle
- imageRef and kernelRef are mutually exclusive on SwiftGuest
- SwiftKernel node opt-in label: kubeswift.io/kernel-node=true
- Kernel artifact localPath is deterministic: /var/lib/kubeswift/kernels/<namespace>-<name>/
- ORAS version in pull jobs: ghcr.io/oras-project/oras:v1.3.1
- Kernel boot pods have no network-init container and no seed volume
- faas-minimal /init must exec with console redirection or use getty for interactive shell