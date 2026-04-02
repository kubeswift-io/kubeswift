# Security Hardening Architecture Notes

> Design decisions, tradeoffs, and Phase 4 considerations for KubeSwift's non-privileged container model.
> Written during QEMU boot validation, April 2, 2026.

## Current Model

KubeSwift launcher pods run with `privileged: false`. Each container drops ALL capabilities
and adds back only what it needs:

| Container | Capabilities | Host Resources |
|-----------|-------------|----------------|
| network-init | NET_ADMIN, NET_RAW | /dev/net/tun (hostPath CharDevice) |
| gpu-init | SYS_ADMIN | /sys/bus/pci (hostPath, sysfs writes for VFIO) |
| launcher (non-GPU) | NET_ADMIN, SYS_ADMIN | /dev/kvm, /dev/net/tun |
| launcher (GPU) | NET_ADMIN, SYS_ADMIN, SYS_RESOURCE, DAC_OVERRIDE | /dev/kvm, /dev/net/tun, /dev/vfio, hugepages |

Pod-level sysctl `net.ipv4.ip_forward=1` enables IP forwarding (namespaced, safe).

## Cluster Prerequisite: Sysctl Allowlist

`net.ipv4.ip_forward` is classified as an "unsafe" sysctl by Kubernetes even though it is
namespaced (scoped to the pod's network namespace, does not affect the host). Clusters must
explicitly allowlist it in kubelet configuration:

**k0s** (`k0s.yaml`):
```yaml
spec:
  workerProfiles:
    - name: default
      values:
        allowedUnsafeSysctls:
          - "net.ipv4.ip_forward"
```

**kubeadm** (`kubelet-config.yaml`):
```yaml
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
allowedUnsafeSysctls:
  - "net.ipv4.ip_forward"
```

**GKE**: Use `--system-config-from-file` with a kubelet config that includes the allowlist.

**EKS**: Set via node group launch template userdata (kubelet extra args).

**AKS**: Use `--kubelet-config` with a JSON file containing `"allowedUnsafeSysctls": ["net.ipv4.ip_forward"]`.

This is the same pattern used by Calico, Cilium, Istio, and other networking components.

## Why Not Alternatives

### Why not `privileged: true` on the launcher?
- Gives full host access — any container escape has root on the node.
- Not allowed by many cluster policies (PodSecurity, OPA/Gatekeeper, Kyverno).
- Overkill — the launcher only needs KVM ioctls and tap device access.

### Why not hostPath `/proc/sys/net`?
- Writes to the **host's** ip_forward, not the pod's network namespace.
- `/proc/sys` doesn't exist as a mountable path on immutable OSes (Bottlerocket, Talos, COS).
- Managed Kubernetes (GKE, EKS, AKS) may block hostPath to `/proc`.
- Security teams flag writable `/proc` mounts.

### Why not a single privileged init container?
- Concentrates privilege but still requires `privileged: true` in the pod spec.
- Breaks Pod Security Standards (restricted/baseline profiles).
- Doesn't reduce the actual attack surface — just time-bounds it.

## Tradeoffs of the Current Approach

### Strengths
- Each permission is documented and justified.
- Pod Security Standards compatible (with sysctl allowlist).
- Works on managed Kubernetes with appropriate node config.
- RBAC, capabilities, and volumes are independently auditable.

### Weaknesses
- **Discovery at runtime**: each new host resource interaction surfaces as a failure
  during testing, requiring a fix cycle. We hit this with `/dev/net/tun` (Bug 33),
  `/proc/sys` (Bug 34), and the sysctl allowlist.
- **Operational complexity**: three different security contexts to maintain across
  three pod builders (disk boot, kernel boot, GPU boot).
- **Cluster configuration requirement**: the sysctl allowlist is a prerequisite that
  must be documented and verified during installation.

## Phase 4 Considerations (Tier 3 HGX Full Passthrough)

Phase 4 adds 8 GPUs + NVSwitches passed to a single VM. Key security implications:

### What scales fine
- **/dev/vfio directory mount**: already handles N VFIO groups. Adding NVSwitches
  means more group files in the same directory. No new volumes needed.
- **/dev/net/tun and sysctl**: one-time costs, apply to all tiers.
- **PCIe topology**: purely QEMU args in the RuntimeIntent JSON. No host permissions.
- **Fabric Manager inside guest** (Tier 3): FM runs in the VM, not the container.
  Zero container-level permission implications.

### What needs attention

**IOMMU group completeness**: VFIO requires ALL devices in an IOMMU group to be bound
to vfio-pci. On HGX boards, a GPU may share an IOMMU group with a PCIe bridge. The
current gpu-init.sh only binds listed GPU addresses. If it misses a group member, VFIO
bind fails at runtime. Fix: gpu-init must enumerate all devices in each IOMMU group
(read `/sys/kernel/iommu_groups/<N>/devices/`) and bind all of them. This is sysfs reads +
writes — same SYS_ADMIN capability, but the logic is more complex.

**Hugepage allocation limits**: 8-GPU VMs may need 2TB+ of hugepages. The launcher's
SYS_RESOURCE capability allows mlock, but Kubernetes resource limits must match. The
controller must set accurate hugepage requests in the pod spec to avoid OOM kills.

**File descriptor limits**: QEMU with 8 GPUs + NVSwitches opens 12-14 VFIO device files
plus control FDs. The default container ulimit (1024) may be insufficient. May need
`spec.containers[].resources` to set the FD limit, or a `securityContext.procMount`
setting. Test empirically on HGX hardware before implementing.

### Recommended approach for Phase 4

1. **Audit-first**: before implementing Phase 4, run `strace` on QEMU with 8 GPUs on a
   real HGX node. Capture every syscall, file open, and ioctl. Build the complete
   permission set from the trace rather than discovering at runtime.

2. **Collapse gpu-init into launcher startup**: the launcher already has SYS_ADMIN.
   Having it also do VFIO binding eliminates one init container and one security context
   to maintain. The gpu-init shell script becomes a function in launcher-entrypoint.sh.
   This reduces the "three security contexts" maintenance burden.

3. **Test IOMMU group handling on real hardware**: mock sysfs can't reproduce real IOMMU
   group layouts (which vary by motherboard BIOS version). Allocate time for on-hardware
   testing before declaring Phase 4 complete.

## Bugs Found During Hardening Validation

| Bug | Root Cause | Fix |
|-----|-----------|-----|
| 32 | `kubeswift.io/guest-hypervisor` annotation never set | Add to report.rs, read in status.go |
| 33 | `/dev/net/tun` not mounted — `ip tuntap add` fails | Add dev-net-tun hostPath CharDevice volume |
| 34 | `/proc/sys` read-only in non-privileged containers | Pod-level sysctl `net.ipv4.ip_forward=1` |
| — | Helm chart missing `gpu.kubeswift.io` RBAC rules | Added to chart ClusterRole |
