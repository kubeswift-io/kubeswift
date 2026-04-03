# QEMU Boot Validation Report

> **Note:** This validation was performed with rust-hypervisor-firmware for CH. CH disk boot has since migrated to CLOUDHV.fd.

> Phase 1 hypervisor abstraction end-to-end test.
> Validates that a VM boots via QEMU, gets networking, and is fully operational.

## Environment

| Property | Value |
|----------|-------|
| Date | 2026-04-03 |
| Kubernetes | v1.34.3+k0s |
| Nodes | frida (control-plane+worker), miles (worker) |
| Node OS | Ubuntu 24.04.4 LTS |
| Node kernels | 6.8.0-106-generic (frida), 6.8.0-101-generic (miles) |
| Container runtime | containerd 1.7.30 |
| Controller image | `ghcr.io/projectbeskar/kubeswift/controller-manager:sha-3c5c476` |
| Swiftletd image | `ghcr.io/projectbeskar/kubeswift/swiftletd:sha-3c5c476` |
| Commit | `3c5c476` (main) |

## Prerequisites

| Resource | Status |
|----------|--------|
| SwiftImage `ubuntu-noble-qemu` | Ready (raw, Noble 24.04 — OVMF/UEFI compatible) |
| SwiftImage `ubuntu-cloud` | Ready (raw, Focal 20.04 — CH disk boot) |
| SwiftGuestClass `default` | Present |
| SwiftSeedProfile `qemu-test-seed` | Present |
| Controller manager | Running (1/1) |

## CH Path Baseline

The Cloud Hypervisor path was verified by the existing `sample` and `faas-test` guests
running concurrently during the QEMU validation:

| Guest | Phase | Hypervisor | IP |
|-------|-------|------------|-----|
| sample | Running | cloud-hypervisor | 10.244.125.17 |
| faas-test | Running | cloud-hypervisor | 10.244.125.11 |

CH path: **PASS** — both guests running with GuestRunning=True and IP assigned.

## QEMU Boot Results

### Test Guest

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: qemu-test
  annotations:
    kubeswift.io/hypervisor-override: "qemu"
spec:
  imageRef:
    name: ubuntu-noble-qemu
  guestClassRef:
    name: default
  seedProfileRef:
    name: qemu-test-seed
  runPolicy: Running
```

### Timing

| Event | Time |
|-------|------|
| Guest applied | T+0s |
| Pod created | T+1s |
| Pod Running | T+9s |
| GuestRunning=True | T+10s |
| IP discovered | T+26s |
| **Total** | **26s** |

### Criterion Results

**Criterion 1: Hypervisor is QEMU** — PASS

```
status.runtime.hypervisor: qemu
RuntimeIntent hypervisor: qemu
Pod annotation kubeswift.io/guest-hypervisor: qemu
```

**Criterion 2: Guest reached Running with GuestRunning=True** — PASS

```
Phase: Running
GuestRunning: True
```

**Criterion 3: Guest IP discovered via DHCP** — PASS

```
Primary IP: 10.244.125.18
```

IP is in the expected 10.244.125.10-20 DHCP range.

**Criterion 4: Console accessible** — PASS

```
Serial socket: /var/lib/kubeswift/run/default-qemu-test/serial.sock
srwxr-xr-x 1 root root 0 Apr  3 09:36 serial.sock
```

### QEMU Command Line

```
qemu-system-x86_64 \
  -name guest=default/qemu-test,debug-threads=on \
  -enable-kvm \
  -machine q35,accel=kvm \
  -cpu host \
  -smp 2 \
  -m 2048M \
  -drive if=pflash,format=raw,readonly=on,file=/usr/share/OVMF/OVMF_CODE.fd \
  -drive if=pflash,format=raw,file=.../OVMF_VARS.fd \
  -drive file=.../image.raw,format=raw,if=virtio \
  -drive file=.../seed.iso,format=raw,if=virtio \
  -netdev tap,id=net0,ifname=tap0,script=no,downscript=no \
  -device virtio-net-pci,netdev=net0,mac=52:54:00:12:34:56 \
  -chardev socket,id=serial0,path=.../serial.sock,server=on,wait=off \
  -serial chardev:serial0 \
  -qmp unix:.../qmp.sock,server=on,wait=off \
  -nographic \
  -monitor none
```

Matches the expected QEMU invocation from the design sketch: Q35 machine, KVM accel,
OVMF firmware, virtio disks, tap networking, serial socket, QMP socket.

## Security Posture

Both network-init and launcher containers run with `privileged: true`. This was a
deliberate decision after the non-privileged approach caused multiple runtime failures
(Bugs 33-34). See [docs/security-hardening-notes.md](../security-hardening-notes.md)
for the full tradeoff analysis.

The gpu-discovery DaemonSet remains non-privileged (read-only sysfs access only).

## Performance Comparison: CH vs QEMU

| Metric | Cloud Hypervisor | QEMU |
|--------|-----------------|------|
| Pod to Running | ~3s | ~9s |
| GuestRunning=True | ~3s | ~10s |
| IP discovery | ~18s | ~26s |
| Guest OS | Ubuntu Focal 20.04 | Ubuntu Noble 24.04 |
| Firmware | rust-hypervisor-firmware | OVMF |
| Boot mode | PVH | UEFI |

QEMU is ~8s slower to IP discovery. Most of the difference is UEFI boot (OVMF) vs
PVH boot (rust-hypervisor-firmware). Noble 24.04 is also a larger image than Focal 20.04.
This overhead is acceptable for GPU workloads where VM lifetime is hours/days.

## Bugs Found and Fixed During Validation

| Bug | Description | Fix | Commit |
|-----|-------------|-----|--------|
| 32 | `kubeswift.io/guest-hypervisor` annotation never set; status hardcoded to "cloud-hypervisor" | Add annotation in report.rs, read in status.go | `c64448f` |
| 33 | `/dev/net/tun` inaccessible without privileged (network-init tap creation failed) | Reverted to privileged:true | `3c5c476` |
| 34 | `/proc/sys/net/ipv4/ip_forward` read-only without privileged (ip forwarding failed) | Reverted to privileged:true | `3c5c476` |
| — | Helm chart ClusterRole missing `gpu.kubeswift.io` RBAC rules | Added to chart | `bb7115d` |

## Conclusion

**QEMU path: VALIDATED.** All four criteria pass. The Phase 1 hypervisor abstraction
works end-to-end:

- swiftletd correctly dispatches to QEMU based on `intent.hypervisor`
- QEMU boots with Q35/OVMF/KVM configuration
- Networking (tap/bridge/dnsmasq) works identically to the CH path
- Status reporting (hypervisor annotation, GuestRunning condition, IP discovery) works
- QMP socket is created and available for lifecycle management
- Serial console socket is accessible

The CH path continues to work correctly (verified by concurrent `sample` and `faas-test` guests).

## Next Steps

1. **Tier 1 GPU validation**: test with a PCIe GPU using `--device path=<sysfs>,x_nv_gpudirect_clique=0` on Cloud Hypervisor
2. **Tier 2 GPU validation**: test with HGX SXM GPUs using QEMU + pcie-root-port topology
3. **Phase 4 implementation**: full PCIe hierarchy for Tier 3 HGX full passthrough
