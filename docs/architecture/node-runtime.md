# Node Runtime

**swiftletd** runs inside each SwiftGuest pod as the launcher container. It reads the runtime intent, builds NoCloud seed (when present), and launches Cloud Hypervisor. There is no separate node daemon—swiftletd is per-pod.

## swiftletd flow

| Step | Action |
|------|--------|
| 1 | Read `runtime-intent.json` from `/var/lib/kubeswift/intent/` |
| 2 | Create runtime dir under `KUBESWIFT_RUN_DIR` (default `/var/lib/kubeswift/run`) |
| 3 | If seed present and not kernel boot: build NoCloud layout via swift-seed into runtime dir |
| 4 | If `lifecycle=stop`: report Stopped, exit |
| 5 | Spawn Cloud Hypervisor (see invocation below) |
| 6 | Poll until CH creates API socket |
| 7 | Patch SwiftGuest `GuestRunning=True` |
| 8 | Wait on CH process; on exit, report Stopped (0) or Failed (non-zero) |

### Cloud Hypervisor invocation

**Disk boot** (when `kernelBoot` is absent in the intent):

```
cloud-hypervisor --api-socket path=<socket> --kernel CLOUDHV.fd \
  --disk path=<root.raw> path=<seed.iso> \
  --memory size=<N>M --cpus boot=<N> \
  --serial socket=<serial.sock> --console off --net tap=tap0
```

**Kernel boot** (when `kernelBoot` is present in the intent):

```
cloud-hypervisor --api-socket path=<socket> \
  --kernel <bzImage> --initramfs <rootfs.cpio.gz> --cmdline "<cmdline>" \
  --memory size=<N>M --cpus boot=<N> \
  --serial socket=<serial.sock> --console off
```

No `--disk`, no `--kernel CLOUDHV.fd`, no `--net` in kernel boot mode.

## Mount paths

### Disk boot pod

| Volume       | Mount path                      | Purpose                          |
|--------------|----------------------------------|----------------------------------|
| root-disk    | `/var/lib/kubeswift/disks/root` | Root disk image (PVC)            |
| seed         | `/var/lib/kubeswift/seed`       | Seed Secret (when present)       |
| runtime-intent | `/var/lib/kubeswift/intent`   | Runtime intent ConfigMap         |
| run          | `/var/lib/kubeswift/run`        | Per-guest runtime directory (emptyDir) |
| dev-kvm      | `/dev/kvm`                      | KVM device passthrough           |

### Kernel boot pod

| Volume           | Mount path                                     | Purpose                          |
|------------------|------------------------------------------------|----------------------------------|
| kernel-artifacts | `/var/lib/kubeswift/kernels/<ns>/<name>/`     | Kernel + initramfs hostPath      |
| runtime-intent   | `/var/lib/kubeswift/intent`                    | Runtime intent ConfigMap         |
| run              | `/var/lib/kubeswift/run`                       | Per-guest runtime directory (emptyDir) |
| dev-kvm          | `/dev/kvm`                                     | KVM device passthrough           |

Kernel boot pods also have `nodeSelector: {"kubeswift.io/kernel-node": "true"}` to ensure they land on nodes where the kernel artifacts exist.

The runtime directory is created under `KUBESWIFT_RUN_DIR` (default `/var/lib/kubeswift/run`). Ensure this path is writable (e.g. emptyDir or hostPath).

## Environment

| Variable               | Default                               | Description                          |
|------------------------|----------------------------------------|--------------------------------------|
| `KUBESWIFT_INTENT_PATH`| `/var/lib/kubeswift/intent/runtime-intent.json` | Path to runtime intent JSON   |
| `KUBESWIFT_RUN_DIR`    | `/var/lib/kubeswift/run`               | Base path for per-guest runtime dirs |
| `KUBESWIFT_CH_BINARY`  | `cloud-hypervisor`                     | Cloud Hypervisor binary path          |
| `POD_NAME`             | (from downward API)                   | SwiftGuest name for status reporting |
| `POD_NAMESPACE`        | (from downward API)                   | SwiftGuest namespace for reporting    |

## Cloud Hypervisor requirements

- **Version:** v54.0 since KubeSwift v0.21.0 (v53.0 before). CLI format: `--api-socket path=`, `--disk path=`, etc.
- **Binary:** Included in the swiftletd container image at `/usr/local/bin/cloud-hypervisor`, checksum-pinned in `images/swiftletd/Containerfile`
- **Socket:** Unix socket only; no TCP binding

### Settings KubeSwift sets explicitly

Cloud Hypervisor v54 changed two defaults. KubeSwift sets both explicitly, so
guests behave as they did on v53:

- **Nested virtualization:** `--cpus ...,nested=on`. Guests see VMX/SVM and can
  use `/dev/kvm`, as on v53 (v54's default is off). This is compatibility
  behaviour; a future API may make it a per-guest choice.
- **virtio-net offloads:** `offload_csum=on,offload_tso=on,offload_ufo=on` on
  every `--net` (tap and vhost-user). With v54's default (off), guest-to-pod
  TCP throughput on the same node fell from about 11 Gbps to under 1 Gbps.

Every file-backed `--disk` carries `image_type=raw` (v54 no longer detects the
image type). These settings are part of the VM's configuration, so a snapshot,
a restore and a live migration keep them, and a guest restored from a v53
snapshot keeps v53's settings.

### Upgrades

A chart upgrade does not change the hypervisor of guests that are already
running. A guest keeps the launcher image it was started with (swiftletd and
Cloud Hypervisor), including across live migration and node drain; it moves to
the new version when its launcher pod is recreated (a stop and start, or an
offline migration). Guests started on v53 therefore stay on v53 until then.
Live-migrating a running guest onto a newer Cloud Hypervisor is not supported.

v54 restores memory snapshots taken on v53 (including `cloneFromSnapshot`).

### Known limitations (v54)

- **Live migration between unlike CPUs:** v54 checks CPU features more
  strictly and may refuse a migration between hosts whose CPUs differ.
  Validated between hosts with identical CPUs only.
- **Unaligned `direct=on` I/O** now fails instead of being emulated. Ubuntu
  24.04 and Windows Server 2025 guests were validated on the disk path
  KubeSwift emits.
- **VM Generation ID:** v54 exposes the device (ACPI `VMGENCTR`). Ubuntu cloud
  images ship its driver as a module that is not loaded, so a guest cloned from
  a memory snapshot does not reseed its RNG unless it loads `vmgenid`. Sandbox
  kernels do not include it; sandboxes are never restored.
- **GPU:** passthrough uses legacy VFIO. v54's iommufd backend works only with
  its new P2P BAR mapping turned off (`vfio_p2p_dma=off`); KubeSwift does not
  use iommufd.
- **No hierarchical PCIe topology:** GPUs that need a PCIe root port per
  device (HGX SXM) still run on QEMU.

## Status reporting

swiftletd patches SwiftGuest status with the `GuestRunning` condition:

- **Running:** `GuestRunning=True`, reason `VmRunning`
- **Stopped:** `GuestRunning=False`, reason `VmStopped`
- **Failed:** `GuestRunning=False`, reason `VmFailed`

Requires RBAC: `patch swiftguests/status`. Apply `config/rbac/` in each namespace where SwiftGuests run.

## Related docs

- [swiftletd MVP](../swiftletd-mvp.md) — Original design notes
- [Seed rendering](../seed-rendering.md) — NoCloud control-plane vs node flow
- [Operator checklist](../operator/operator-checklist-ubuntu-x86_64.md) — Host prerequisites including `/dev/kvm`
