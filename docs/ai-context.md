# KubeSwift Project Context
> This document is the canonical context anchor for AI-assisted KubeSwift development.
> It should be read at the start of every new session before any work begins.
> Last updated: March 18, 2026 — swiftctl describe/logs, image pipeline improvements completed

---

## What is KubeSwift

KubeSwift is a Kubernetes-native virtual machine runtime built on Cloud Hypervisor.
It allows Kubernetes users to run virtual machines as first-class Kubernetes workloads,
using custom resources and controllers.

KubeSwift is **not** a container sandbox (not Kata Containers).
It is a VM platform where virtual machines are first-class Kubernetes workloads.

It is similar in spirit to KubeVirt but with a much simpler architecture:
- Runtime: Cloud Hypervisor (not QEMU)
- Firmware: rust-hypervisor-firmware v0.5.0
- Distribution: OCI-native (Helm chart + container images)
- Goal: minimal architecture, strong operability, fast iteration

Repository: `https://github.com/projectbeskar/kubeswift` (private)
Images: `ghcr.io/projectbeskar/kubeswift/` (public packages)

---

## Current State (v0.1.0-smoke-passed)

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

### Known working configuration
- Guest OS: **Ubuntu Focal (20.04)** cloud image — Noble (24.04) is incompatible with rust-hypervisor-firmware due to EFI protocol gaps (TPM, MOK, writable NVRAM)
- Firmware: rust-hypervisor-firmware v0.5.0, loaded via `--kernel` flag (NOT `--firmware`)
- Cloud Hypervisor: v51.1
- Seed format: NoCloud flat layout (meta-data, user-data, network-config at ISO root)
- Seed ISO: genisoimage with `-rock -joliet -volid cidata` flags
- DHCP range: 10.244.125.10–20 on br0 (10.244.125.1)

### Deployed images
- `ghcr.io/projectbeskar/kubeswift/controller-manager:sha-5b757de`
- `ghcr.io/projectbeskar/kubeswift/swiftletd:sha-5b757de`

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
 ├─ init container: network-init (bridge/tap/dnsmasq setup)
 └─ launcher container: swiftletd (Rust)
        │
        ▼
     Cloud Hypervisor v51.1
        │  --kernel hypervisor-fw (rust-hypervisor-firmware v0.5.0)
        ▼
      Guest VM (Ubuntu Focal)
```

### CRDs

**SwiftGuest** — represents a running VM
```yaml
spec:
  imageRef:       # SwiftImage to boot from
  guestClassRef:  # SwiftGuestClass for resource defaults
  seedProfileRef: # SwiftSeedProfile for cloud-init
  runPolicy:      # Running | Stopped
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
    primaryIP:    # discovered from dnsmasq DHCP lease
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
```

### Repository Structure
```
api/                        CRD types (Go)
cmd/
  swiftctl/                 CLI (console, ssh, describe, logs, start, stop, restart, debug)
internal/
  controller/swiftguest/    SwiftGuest controller
  controller/swiftimage/    SwiftImage controller (import + measure jobs)
  runtimeintent/            VM launch spec builder
  seed/                     cloud-init ConfigMap builder
  cli/                      shared CLI helpers

rust/
  swiftletd/                VM launcher (main binary in swiftletd image)
  swift-runtime/            RuntimeDir management
  swift-ch-client/          Cloud Hypervisor spawn + API client
  swift-seed/               NoCloud ISO builder

images/
  swiftletd/Containerfile   Multi-stage: rust builder + debian runtime
  controller-manager/
  webhook-server/

config/
  crd/bases/                Generated CRD YAML (apply with kubectl apply -k config/crd)
  samples/                  Sample SwiftImage, SwiftGuest, etc.

charts/kubeswift/           Helm chart
test/smoke/boot-test.sh     End-to-end smoke test
```

### Networking Model
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

### Image Import Pipeline
```
SwiftImage created
  │
  ▼
Import Job (ubuntu:22.04, privileged)
  │  curl download
  │  qemu-img convert qcow2 → raw (if needed)
  │  GPT partition detection via od (NOT fdisk — fails on GPT)
  │  mount each partition, patch grub.cfg:
  │    - add console=ttyS0 to kernel cmdline (if missing)
  │    - add serial --speed=115200 BEFORE terminal_input/output
  │    - replace terminal_input/output console → serial console
  │  stat -c %s image.raw > image.raw.size
  ▼
Measure Job (ubuntu:22.04)
  │  cat /data/image.raw.size → parsed as int64
  ▼
image.raw stored in PVC
status.phase = Ready
status.sourceFormat = qcow2
status.preparedFormat = raw
status.preparedArtifact.size = <measured bytes>
```

Key facts:
- Runtime disk format must be **raw** (Cloud Hypervisor requirement)
- GRUB terminal patch order matters: `serial` init must come BEFORE `terminal_input serial`
- Ubuntu Noble (24.04) does NOT boot with rust-hypervisor-firmware — use Focal (20.04)
- Import job uses `od`-based GPT LBA parser (not fdisk) for partition detection

### Cloud Hypervisor Invocation
```
cloud-hypervisor \
  --kernel /usr/share/kubeswift-firmware/hypervisor-fw \   # rust-hypervisor-firmware v0.5.0
  --api-socket path=<runtime-dir>/ch.sock \
  --memory size=2048M \
  --cpus boot=2 \
  --disk path=<image.raw> path=<seed.iso> \
  --serial socket=<runtime-dir>/serial.sock \
  --console off \
  --net tap=tap0
```

Critical: `--kernel` is correct for rust-hypervisor-firmware (PVH ELF binary).
`--firmware` is for OVMF/EDK2 only — using it silently prevents serial output.

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

### Smoke Test
```bash
make smoke-test
# or with custom timeouts:
./test/smoke/boot-test.sh --timeout-image 15 --timeout-guest 5 --timeout-network 5
```

Success criteria:
- `GuestRunning=True`
- `status.network.primaryIP` populated

---

## Roadmap

### Completed (v0.1.0)
- VM boots end-to-end ✓
- Networking works ✓
- IP discovery and status reporting ✓
- swiftctl console ✓
- swiftctl start/stop/restart/debug ✓
- Smoke test passes ✓
- swiftctl ssh <guest> with --user and --identity flags ✓
- Rich guest status: runtime.pid, runtime.hypervisor, console.serialSocket, network.interfaces[] ✓
- Graceful stop via SIGTERM + 30s fallback to pod delete ✓
- RestartPolicy=Never on launcher pod ✓
- Controller stopped guard — no pod recreation when runPolicy=Stopped ✓
- Image pipeline improvements: sourceFormat, preparedFormat, size measurement ✓
- swiftctl describe <guest> ✓
- swiftctl logs <guest> ✓

### Next Priorities (in order)

**1. Full lifecycle controls (remaining)**
- runPolicy: RestartOnFailure | Always
- Controller handles VM crash/exit and requeues

**2. SwiftKernel — MicroVM Kernel Library**
- Blocked on kernel build pipeline (buildroot + Cloud Hypervisor reference config)
- Target profiles: faas-minimal, gpu-workload, vhost-user
- Build pipeline to live at build/kernels/faas-minimal/ in repo
- Use ORAS Go library for OCI artifact pull in SwiftKernel controller
- HTTP download as interim path, ORAS as production path
- See SwiftKernel Build Notes section for full requirements

**3. Host runtime hardening**
- Drop unnecessary privileges in launcher pod
- Restrict host mounts

**4. Documentation**
- README (what KubeSwift is, how it differs from KubeVirt)
- docs/install.md, quickstart.md, networking.md, images.md, swiftctl.md, architecture.md

**5. Observability**
- Metrics: kubeswift_guest_running_total, kubeswift_vm_boot_seconds, kubeswift_vm_failures_total
- Structured logging improvements in controller and swiftletd

### Long-term (not yet prioritized)
- Live migration
- Snapshots / persistent disks
- SR-IOV / Multus networking
- GPU passthrough
- High availability controllers

---

## SwiftKernel Build Notes

### Kernel requirements
- Base: Cloud Hypervisor reference config at resources/linux-config-x86_64
- Required: CONFIG_VIRTIO*, CONFIG_SERIAL_8250_CONSOLE, CONFIG_VIRTIO_NET, CONFIG_VIRTIO_BLK
- Build tool: buildroot (produces both bzImage and initramfs in one build)

### Initramfs requirements
- BusyBox statically linked (musl)
- Minimal /init script: mount /proc /sys, networking, exec workload
- No systemd, no cloud-init, no package manager

### OCI packaging
- Use ORAS CLI to push bzImage + initramfs.cpio.gz as OCI artifacts
- Artifact type: application/vnd.kubeswift.kernel.v1
- Target: ghcr.io/projectbeskar/kubeswift/kernels/faas:6.6.0

### Boot verification (must pass before KubeSwift integration)
- cloud-hypervisor --kernel bzImage --initramfs initramfs.cpio.gz
- --cmdline "console=ttyS0 root=/dev/ram0" --memory size=256M --cpus boot=1
- Must give interactive shell before touching KubeSwift code

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

---

## AI Assistant Instructions

When helping develop KubeSwift:

- Read this document and the session transcript before starting any work
- Check `/mnt/transcripts/journal.txt` for previous session summaries
- Prefer minimal changes — one bug fix at a time, verified with real output
- Always ask for actual cluster output before suggesting fixes
- Never assume a fix worked without seeing logs confirming it
- When writing Cursor prompts: be explicit about what NOT to change
- CRD changes always require `make generate manifests` + `kubectl apply -k config/crd` — remind the user
- The working guest OS is Ubuntu Focal — do not suggest Noble without noting incompatibility
- rust-hypervisor-firmware uses `--kernel`, not `--firmware`
- The GRUB terminal patch order is: serial init → terminal_input → terminal_output
- swiftletd reports status via pod annotations, not direct SwiftGuest status patches
- RestartPolicy on launcher pods is always Never — controller owns VM lifecycle