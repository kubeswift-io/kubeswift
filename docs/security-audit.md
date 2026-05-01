# KubeSwift Security Audit Report

**Date:** April 1, 2026
**Scope:** All components touching host resources, elevated privileges, or trust boundaries
**Auditor:** Security Engineer (review only -- no code changes)
**Codebase:** main branch at commit 5f3b3e1

---

## Summary Table

| ID | Severity | Component | Finding |
|----|----------|-----------|---------|
| SEC-01 | ~~CRITICAL~~ RESOLVED | launcher container | ~~`privileged: true`~~ Replaced with drop ALL + NET_ADMIN, SYS_ADMIN (non-GPU) / + SYS_RESOURCE, DAC_OVERRIDE (GPU) |
| SEC-02 | ~~CRITICAL~~ RESOLVED | network-init container | ~~`privileged: true`~~ Replaced with drop ALL + NET_ADMIN, NET_RAW |
| SEC-03 | ~~HIGH~~ RESOLVED | gpu-init container | ~~`privileged: true`~~ Replaced with drop ALL + SYS_ADMIN; sysfs-pci hostPath volume added |
| SEC-04 | HIGH (mitigated) | /dev/vfio hostPath | Entire `/dev/vfio` directory mounted — documented why scoping is impractical (VFIO group files created during bind) |
| SEC-05 | ~~HIGH~~ RESOLVED | PCI address injection | BDF format validation added to gpu-init.sh (regex: `^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$`) |
| SEC-06 | ~~HIGH~~ RESOLVED | Fabric Manager partition isolation | Partition ownership validated against SwiftGPUNode allocatedTo before pod creation |
| SEC-07 | MEDIUM | /dev/kvm hostPath | Mounted as hostPath volume, not a proper device volume |
| SEC-08 | MEDIUM | RBAC over-permissioning | Controller ClusterRole grants `delete` on SwiftGPUProfiles and SwiftGPUNodes unnecessarily |
| SEC-09 | MEDIUM | RBAC cluster-wide secrets read | Controller can `get/list/watch` all Secrets cluster-wide |
| SEC-10 | MEDIUM | DaemonSet swiftletd | Runs with `privileged: true`, `hostPID: true`, `hostNetwork: true` |
| SEC-11 | MEDIUM | GPU allocation race condition | No optimistic concurrency lock on SwiftGPUNode allocation updates |
| SEC-12 | MEDIUM | RuntimeIntent as ConfigMap | Attacker with ConfigMap write access can inject arbitrary hypervisor commands |
| SEC-13 | MEDIUM | Static MAC address | All QEMU VMs use hardcoded MAC `52:54:00:12:34:56`, causing L2 conflicts |
| SEC-14 | LOW | SwiftKernel pull secret scoping | `imagePullSecrets` used instead of registry auth for ORAS; secret name from user-supplied spec |
| SEC-15 | LOW | Kernel pull job runs as root | Pull job uses `runAsUser: 0` for hostPath write; could be scoped tighter |
| SEC-16 | LOW | VFIO bind interrupted state | gpu-init.sh unbind/rebind is not atomic; crash leaves device driverless |
| SEC-17 | LOW | Guest-to-pod network reachability | Bridge subnet is hardcoded; no network policy enforcement on guest traffic |
| SEC-18 | LOW | No seccomp profile | No containers specify a seccomp profile |
| SEC-19 | LOW | OVMF_VARS template world-readable | Writable UEFI variable store copied without restrictive permissions |

---

## Detailed Findings

### SEC-01: Launcher container runs with `privileged: true` [CRITICAL]

**Current state:** All three pod builders (`buildDiskBootPod`, `buildKernelBootPod`, `BuildGPUDiskBootPod` in `internal/controller/swiftguest/pod.go` and `gpu.go`) set `Privileged: ptr.To(true)` on the launcher container.

**Files:** `internal/controller/swiftguest/pod.go:111-112`, `pod.go:229-230`, `gpu.go:400-401`

**Risk:** `privileged: true` disables all Linux security boundaries: it grants all capabilities (including `SYS_ADMIN`, `SYS_RAWIO`, `NET_ADMIN`, `SYS_PTRACE`), disables seccomp, grants access to all host devices, and mounts sysfs read-write. A container escape from swiftletd or the hypervisor process gives full root access to the host.

**Blast radius:** Full node compromise. An attacker who escapes the VM can access all host devices, all other pods' data, and the kubelet credentials.

**Minimum capabilities actually needed by swiftletd:**
- `/dev/kvm` access: Already mounted explicitly. Requires no special capability if the device file is group-accessible (add container user to `kvm` group), or use a device plugin.
- `/dev/net/tun` for TAP: `NET_ADMIN` capability (already done by network-init; launcher does not create TAP devices itself).
- Process management (hypervisor child): No special capability needed.
- Annotation patching (Kubernetes API): No special capability needed (service account RBAC).
- `/dev/vfio/*` access (GPU path): Requires the VFIO character device to be accessible. Can use `SYS_RAWIO` + explicit device cgroup rules instead of privileged.
- Hugepages: Mounted as emptyDir volumes; no capability needed.

**Recommended fix:** Replace `privileged: true` with:
```yaml
securityContext:
  privileged: false
  capabilities:
    add: ["NET_ADMIN", "SYS_RAWIO"]
    drop: ["ALL"]
  runAsUser: 0
```
For the non-GPU path, only `NET_ADMIN` may be needed (for dnsmasq started by entrypoint). Test incrementally: harden the non-GPU disk-boot path first, then kernel boot, then GPU boot.

---

### SEC-02: network-init container is `privileged: true` despite only needing `NET_ADMIN` [CRITICAL]

**Current state:** `pod.go:83-88` and `gpu.go:352-360` set both `Privileged: ptr.To(true)` and `Capabilities.Add: ["NET_ADMIN"]`. When `privileged` is true, the capabilities field is ignored -- the container gets all capabilities regardless.

**File:** `internal/controller/swiftguest/pod.go:83-88`

**Script analysis:** `network-init.sh` performs:
- `ip link add ... type bridge` -- requires `NET_ADMIN`
- `ip addr add` -- requires `NET_ADMIN`
- `ip tuntap add` -- requires `NET_ADMIN`
- `echo 1 > /proc/sys/net/ipv4/ip_forward` -- requires `NET_ADMIN` (or write to sysctl)
- `iptables -t nat -A POSTROUTING` -- requires `NET_ADMIN` + `NET_RAW`

**Risk:** Identical to SEC-01. The network-init container runs a simple shell script that finishes in under a second, but during that time it has full host access.

**Blast radius:** Full node compromise if the container image is tampered with or if a supply-chain attack injects code into the init container.

**Recommended fix:**
```yaml
securityContext:
  privileged: false
  capabilities:
    add: ["NET_ADMIN", "NET_RAW"]
    drop: ["ALL"]
  readOnlyRootFilesystem: true
  runAsUser: 0
```
`NET_RAW` is needed for iptables. `NET_ADMIN` covers all the `ip` commands and sysctl writes within the network namespace. No other capabilities are needed.

---

### SEC-03: gpu-init container runs `privileged: true` [HIGH]

**Current state:** `gpu.go:339-341` sets `Privileged: ptr.To(true)` on the gpu-init container.

**Script analysis:** `gpu-init.sh` performs:
- Write to `/sys/bus/pci/devices/<addr>/driver/unbind` -- requires sysfs write access
- Write to `/sys/bus/pci/devices/<addr>/driver_override` -- requires sysfs write access
- Write to `/sys/bus/pci/drivers_probe` -- requires sysfs write access
- Read symlink at `/sys/bus/pci/devices/<addr>/driver` -- read access
- Execute `fmpm -a <partition_id>` -- requires access to NVIDIA management interface

**Risk:** The gpu-init container needs write access to specific sysfs paths and execution of fmpm. `privileged: true` grants far more than this.

**Blast radius:** During the init container's execution window, a compromised container has full host access. The gpu-init also has `/dev/vfio` mounted.

**Recommended fix:** This is the hardest container to harden because sysfs writes for driver binding are gated behind `SYS_ADMIN` in the default kernel. Options:
1. Use `privileged: false` with `capabilities: add: ["SYS_ADMIN"]` and mount only the specific sysfs paths needed. `SYS_ADMIN` is still broad but better than `privileged`.
2. Move VFIO binding to a host-level agent (DaemonSet) that pre-binds devices, avoiding the need for privileged init containers entirely.
3. Use a device plugin that handles VFIO binding at the kubelet level.

Risk of `SYS_ADMIN`: Still allows mount namespace manipulation, but combined with `drop: ["ALL"]` and `readOnlyRootFilesystem`, the attack surface is reduced compared to `privileged: true`.

---

### SEC-04: /dev/vfio hostPath is directory-scoped, not group-scoped [HIGH]

**Current state:** `gpu.go:289-296` mounts the entire `/dev/vfio` directory into the GPU launcher pod.

```go
{
    Name: "dev-vfio",
    VolumeSource: corev1.VolumeSource{
        HostPath: &corev1.HostPathVolumeSource{
            Path: "/dev/vfio",
            Type: ptr.To(corev1.HostPathDirectory),
        },
    },
}
```

**Risk:** `/dev/vfio/` contains one character device per IOMMU group (`/dev/vfio/1`, `/dev/vfio/2`, etc.) plus the control device `/dev/vfio/vfio`. Mounting the entire directory gives the launcher container access to ALL IOMMU groups on the node, not just the groups for the allocated GPUs. If another tenant's GPU is in a different IOMMU group on the same node, this container can open that IOMMU group's character device.

**Blast radius:** Cross-tenant GPU access on multi-tenant GPU nodes. A guest VM could theoretically perform DMA to memory regions belonging to another tenant's IOMMU group (though IOMMU isolation should prevent this at the hardware level if properly configured).

**Recommended fix:** Mount only the specific VFIO group device files needed. The controller knows the IOMMU groups from `SwiftGPUNode.status.gpus[].iommuGroup`. Generate per-GPU volume mounts:
```yaml
volumes:
  - name: vfio-group-15
    hostPath:
      path: /dev/vfio/15
      type: CharDevice
  - name: vfio-control
    hostPath:
      path: /dev/vfio/vfio
      type: CharDevice
```

---

### SEC-05: No validation of PCI BDF addresses in QEMU command-line construction [HIGH]

**Current state:** PCI addresses flow from `SwiftGPUNode.status.gpus[].PCIAddress` (populated by the discovery DaemonSet) through the controller into `RuntimeIntent.GPU.Devices[].PCIAddress`, and then into QEMU command-line arguments via `swift-qemu-client/config.rs`.

In `gpu.go:71`:
```go
PCIAddress: pciAddr,
```

In `config.rs:85-86`:
```rust
format!("tap,id=net0,ifname={},script=no,downscript=no", tap),
```

And for VFIO devices, the PCI address is interpolated directly into `-device vfio-pci,host=<address>`.

**Risk:** If the discovery DaemonSet is compromised or a malicious actor gains write access to SwiftGPUNode status, they can inject arbitrary strings into the PCI address field. While QEMU itself validates PCI addresses, the broader concern is that string fields from CRD status are used to build command-line arguments without sanitization. A crafted PCI address like `0000:17:00.0,rombar=1,x-vga=on` could inject additional QEMU device parameters.

**Blast radius:** Potential for QEMU misconfiguration or security bypass (e.g., enabling VGA passthrough, ROM BAR access, or other device options not intended by the profile).

**Recommended fix:** Add a PCI BDF format validation regex in the Go controller before writing to RuntimeIntent:
```go
var pciAddrRegex = regexp.MustCompile(`^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$`)
```
Reject any address that does not match. Also validate on the Rust side before interpolation.

---

### SEC-06: Fabric Manager partition isolation -- no cross-tenant authorization [HIGH]

**Current state:** The SwiftGPU controller selects a Fabric Manager partition in `allocate.go:87-93`:
```go
if profile.Spec.PartitionMode == "shared" {
    partID, err = findFMPartition(n.Status.FabricManager, profile.Spec.Count)
}
```

The partition ID is passed to the gpu-init container via environment variable `GPU_PARTITION_ID`, which runs `fmpm -a <partition_id>`.

**Risk:**
1. The controller selects partitions solely based on GPU count match and `AllocatedTo == ""`. There is no check that the partition's GPU indices correspond to the GPUs actually allocated to this guest. A partition containing GPUs 0-1 could be activated while GPUs 2-3 were allocated.
2. If an attacker can modify the `GPU_PARTITION_ID` environment variable (e.g., by patching the pod spec before it starts), they could activate any partition on the node, including one containing GPUs allocated to another tenant.
3. `fmpm -a` runs with no authentication. The gpu-init script trusts the partition ID blindly.

**Blast radius:** Cross-tenant NVLink fabric access. If partition X is activated with GPUs belonging to tenant A, tenant B's VM gets NVLink connectivity to tenant A's GPUs.

**Recommended fix:**
1. Validate that the selected partition's `GPUIndices` exactly match the GPU indices allocated to this guest.
2. Consider having the controller itself (or a trusted host agent) activate the partition rather than passing the ID through a pod environment variable.
3. Add idempotency checks: verify the partition is not already active for another tenant before activation.

---

### SEC-07: /dev/kvm mounted as hostPath instead of device volume [MEDIUM]

**Current state:** All pod builders mount `/dev/kvm` as a `HostPath` volume with type `CharDevice`.

```go
{
    Name: "dev-kvm",
    VolumeSource: corev1.VolumeSource{
        HostPath: &corev1.HostPathVolumeSource{
            Path: "/dev/kvm",
            Type: ptr.To(corev1.HostPathType("CharDevice")),
        },
    },
}
```

**Risk:** While `HostPathType("CharDevice")` validates the path is a character device at pod creation, hostPath volumes bypass the device plugin framework and are not tracked by the kubelet's device allocation system. This means:
- Multiple pods can open `/dev/kvm` simultaneously without resource accounting.
- Pod Security Standards (Restricted profile) block hostPath volumes entirely.
- No integration with device health monitoring.

**Blast radius:** Low direct security risk since KVM is designed for multi-tenant use. However, this blocks adoption of Pod Security Standards and prevents proper resource tracking.

**Recommended fix:** Use the KVM device plugin (`github.com/kubevirt/kubernetes-device-plugins/cmd/kvm`) which exposes `devices.kubevirt.io/kvm` as a schedulable resource. This integrates with PSS Restricted profile and enables proper resource accounting.

---

### SEC-08: RBAC over-permissioning on controller ClusterRole [MEDIUM]

**Current state:** `config/manager/controller-manager-rbac.yaml` grants:

| Resource | Verbs | Issue |
|----------|-------|-------|
| `swiftgpuprofiles` | `get, list, watch, create, update, patch, delete` | Controller only reads profiles; does not need `create/update/patch/delete` |
| `swiftgpunodes` | `get, list, watch, create, update, patch, delete` | Controller patches status only; does not need `create/delete` on the main resource (discovery DaemonSet creates them) |
| `secrets` | `get, list, watch` | Cluster-wide secret read (see SEC-09) |
| `persistentvolumeclaims` | full CRUD | Controller only creates PVCs for image import; may not need `update/patch` |

**Risk:** Principle of least privilege violation. If the controller's service account token is compromised, the attacker can delete GPU profiles and GPU node inventory, disrupting the entire cluster's GPU scheduling.

**Blast radius:** Cluster-wide disruption of GPU workload scheduling if attacker deletes SwiftGPUNode resources.

**Recommended fix:**
- `swiftgpuprofiles`: `get, list, watch` only
- `swiftgpunodes`: `get, list, watch` for the resource; `get, update, patch` for status subresource (already correct for status)
- Remove `create, delete` from `swiftgpunodes` main resource verbs
- Scope PVC verbs to `get, list, watch, create, delete` (no update/patch needed)

---

### SEC-09: Cluster-wide secrets read access [MEDIUM]

**Current state:** The controller ClusterRole grants `get, list, watch` on all Secrets cluster-wide:
```yaml
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["get", "list", "watch"]
```

**Risk:** The controller only needs secret access for SwiftKernel pull secrets (OCI registry credentials). Cluster-wide secret read access means the controller can read any secret in any namespace, including TLS certificates, database passwords, and other sensitive credentials.

**Blast radius:** If the controller is compromised, all secrets in the cluster are exposed.

**Recommended fix:** The controller should use namespace-scoped Role/RoleBinding for secret access in namespaces where SwiftKernels are created, rather than a ClusterRole. Alternatively, use a more targeted approach where the controller only reads the specific secret named in `spec.ociRef.pullSecret`.

---

### SEC-10: DaemonSet swiftletd runs with excessive host access [MEDIUM]

**Current state:** `config/daemonset/daemonset.yaml` runs with:
```yaml
hostPID: true
hostNetwork: true
securityContext:
  privileged: true
```
Plus mounts `/var/lib/kubeswift` as a hostPath.

**Risk:** This DaemonSet has full access to host processes (hostPID), the host network stack (hostNetwork), all devices and capabilities (privileged), and persistent host storage. A container escape provides immediate full node compromise with no additional privilege escalation needed.

**Blast radius:** Full compromise of every node where the DaemonSet runs. With hostPID, the attacker can inspect and signal any process on the host. With hostNetwork, they can sniff all network traffic.

**Recommended fix:** Evaluate whether this DaemonSet is actually used in the current architecture. The comment in the file says "alternative deployment models." If it is not needed for the launcher-pod architecture, remove it or mark it as experimental. If retained, apply the same capability restrictions recommended in SEC-01 and remove `hostPID: true` (swiftletd does not need to see host processes).

---

### SEC-11: GPU allocation race condition -- no optimistic locking [MEDIUM]

**Current state:** `allocate.go:117` updates SwiftGPUNode status after marking GPUs as allocated:
```go
if err := r.Status().Update(ctx, n); err != nil {
    return nil, nil, nil, -1, fmt.Errorf("update SwiftGPUNode %s status: %w", n.Name, err)
}
```

The `r.Status().Update()` call uses the resource version from the `List` call that fetched the node list. However, between the `List` and the `Update`, another reconcile loop (for a different SwiftGuest) could also be allocating GPUs on the same node.

**Risk:** Two concurrent reconcile loops could read the same SwiftGPUNode state (both see FreeGPUs=4), both select the same GPUs, and both attempt to mark them as allocated. The second `Update` will fail due to resource version conflict, which is handled by returning an error and retrying. However, the idempotency guard in `findAndAllocate` checks `AllocatedTo == allocatedTo` (the current guest's name), so a retry would correctly detect the first guest's allocation. The second guest would then get "no capacity" until the next reconcile sees fresh state.

This is not a correctness bug (double allocation) because the Kubernetes API server enforces optimistic concurrency on `Update`. But it can cause unnecessary delays and error logs.

**Blast radius:** Allocation delays under concurrent GPU scheduling; no actual double-allocation.

**Recommended fix:** The current design is safe due to Kubernetes optimistic concurrency. For improved observability, log the conflict error distinctly from other errors so operators know it is benign. Consider using `Patch` instead of `Update` for partial status updates to reduce conflict surface.

---

### SEC-12: RuntimeIntent ConfigMap as attack surface [MEDIUM]

**Current state:** The RuntimeIntent is serialized as a JSON ConfigMap by the controller and mounted read-only into the launcher pod. swiftletd reads this ConfigMap to determine which hypervisor to run and with what arguments.

**Risk:** If an attacker has `update` or `patch` permission on ConfigMaps in the guest's namespace, they can modify the RuntimeIntent after the controller creates it but before the pod starts. This could:
1. Change `hypervisor` to `qemu` when `cloud-hypervisor` was intended (or vice versa)
2. Inject arbitrary file paths into `rootDisk.path` or `seedPath`, potentially reading host files
3. Modify `kernelBoot.cmdline` to boot with attacker-controlled kernel parameters
4. Change the `guestId` to impersonate another guest's status reporting

The Rust side (`intent.rs`) deserializes the JSON with serde and passes values directly to hypervisor command construction without additional validation.

**Blast radius:** Guest VM compromise; potential for host file reads if disk paths are crafted to point at host-mounted volumes.

**Recommended fix:**
1. Add an integrity annotation (HMAC or hash) to the ConfigMap that swiftletd validates before consuming the intent.
2. Use an immutable ConfigMap (Kubernetes 1.21+: `immutable: true`) to prevent modification after creation.
3. Validate all file paths in swiftletd against a known-safe prefix (e.g., `/var/lib/kubeswift/`).

---

### SEC-13: Static MAC address causes L2 collisions [MEDIUM]

**Current state:** `launch.rs:162` hardcodes the MAC address for all QEMU VMs:
```rust
mac: "52:54:00:12:34:56".to_string(),
```

**Risk:** All QEMU-path VMs on the same node will have identical MAC addresses. If multiple GPU VMs run on the same node (each with their own bridge), this is not an issue. But if the bridge model ever changes to a shared bridge, or if VMs are on the same L2 segment, MAC collisions cause unpredictable networking -- ARP confusion, packet duplication, and traffic misdirection.

**Blast radius:** Network disruption between co-located QEMU VMs if bridge isolation assumptions change.

**Recommended fix:** Generate a unique locally-administered MAC per VM, derived from the guest ID:
```rust
fn generate_mac(guest_id: &str) -> String {
    let hash = sha256(guest_id);
    format!("52:54:00:{:02x}:{:02x}:{:02x}", hash[0], hash[1], hash[2])
}
```

---

### SEC-14: SwiftKernel pull secret scoping [LOW]

**Current state:** `pull.go:89-92` uses `imagePullSecrets` on the pod spec:
```go
if sk.Spec.OCIRef.PullSecret != "" {
    podSpec.ImagePullSecrets = []corev1.LocalObjectReference{
        {Name: sk.Spec.OCIRef.PullSecret},
    }
}
```

**Risk:** `imagePullSecrets` is for pulling container images, not for OCI artifacts pulled by ORAS. ORAS uses its own registry authentication, which typically reads from `~/.docker/config.json` or environment variables. The `imagePullSecrets` setting here does not actually provide credentials to the ORAS CLI running inside the container. This means:
1. Private registry pulls may fail silently (ORAS has no credentials).
2. The secret name is user-supplied and not validated -- a user could reference a secret in their namespace that contains credentials for a different registry, though this is a normal Kubernetes pattern.

**Blast radius:** Minimal. The pull secret is namespace-scoped (same namespace as the SwiftKernel), which is the correct Kubernetes trust boundary.

**Recommended fix:** If private OCI registries are needed, mount the pull secret as a volume and configure ORAS to read credentials from it, or use `oras login` in the pull script with credentials from the mounted secret.

---

### SEC-15: Kernel pull job runs as root [LOW]

**Current state:** `pull.go:67-69`:
```go
SecurityContext: &corev1.PodSecurityContext{
    RunAsUser: ptr.To(int64(0)),
},
```

**Risk:** The pull job runs as root to write to the hostPath `/var/lib/kubeswift/kernels/`. Running as root in a container is unnecessary if the host directory has appropriate ownership/permissions.

**Blast radius:** Low. The container has no privileged capabilities and only mounts one hostPath directory. A container escape would give root access only to that directory, not the full host.

**Recommended fix:** Pre-create `/var/lib/kubeswift/kernels/` with ownership matching a non-root UID (e.g., 1000) via a DaemonSet or node setup, then run the pull job as that UID.

---

### SEC-16: VFIO bind interrupted state [LOW]

**Current state:** `gpu-init.sh` performs a three-step driver rebind:
```bash
echo "${addr}" > /sys/bus/pci/devices/"${addr}"/driver/unbind 2>/dev/null || true
echo vfio-pci > /sys/bus/pci/devices/"${addr}"/driver_override
echo "${addr}" > /sys/bus/pci/drivers_probe
```

If the script is interrupted (SIGKILL, OOM, node crash) between the `unbind` and the `drivers_probe`, the device is left without any driver bound. The `driver_override` is subsequently cleared, so a future `drivers_probe` or `modprobe` would bind the original driver again, but until then the device is orphaned.

**Risk:** A GPU left driverless is unavailable to both the host and any VM. Requires manual intervention (`echo 1 > /sys/bus/pci/devices/<addr>/rescan`) or a reboot to recover.

**Blast radius:** Single GPU unavailable until manual recovery. Does not affect other GPUs or system stability.

**Recommended fix:** Add a cleanup trap in the script:
```bash
cleanup() {
    for addr in "${ADDRS[@]}"; do
        echo > /sys/bus/pci/devices/"${addr}"/driver_override 2>/dev/null || true
    done
}
trap cleanup EXIT
```
This ensures `driver_override` is always cleared even on interruption, allowing the kernel's default driver binding to take over.

---

### SEC-17: Guest-to-pod network reachability [LOW]

**Current state:** `network-init.sh` creates an iptables MASQUERADE rule:
```bash
iptables -t nat -A POSTROUTING -s 192.168.99.0/24 ! -d 192.168.99.0/24 -j MASQUERADE
```

This NATs guest traffic to the pod's `eth0` IP, giving the guest access to the entire pod network (all services, all pods in the cluster).

**Risk:** A guest VM can reach any Kubernetes service or pod that the launcher pod can reach. There are no iptables FORWARD rules restricting which destinations the guest can access. The guest can reach the Kubernetes API server, the cluster DNS, and any service endpoint.

**Blast radius:** A compromised guest VM has the same network-level access as the launcher pod. If the launcher pod's service account token is accessible from inside the guest (it is not -- it is only in the launcher container's filesystem), this is not directly exploitable. But the guest can reach cluster services.

**Recommended fix:**
1. Add iptables FORWARD rules to restrict guest traffic to specific CIDRs (e.g., block access to the Kubernetes API server CIDR, pod CIDR of other namespaces).
2. Apply NetworkPolicy on the launcher pod to restrict egress.
3. Consider using a dedicated network namespace for the bridge to further isolate guest traffic.

---

### SEC-18: No seccomp profile on any container [LOW]

**Current state:** None of the pod builders specify a seccomp profile. When `privileged: true` is set, seccomp is disabled entirely. Even when privileged is removed (per SEC-01/02/03 recommendations), the default seccomp profile must be explicitly set.

**Risk:** Without seccomp, syscalls like `mount`, `reboot`, `kexec_load`, and `clock_settime` are available to the container process.

**Blast radius:** Increases the attack surface for container escape exploits that rely on unusual syscalls.

**Recommended fix:** Apply `RuntimeDefault` seccomp profile to all containers once `privileged: true` is removed:
```yaml
securityContext:
  seccompProfile:
    type: RuntimeDefault
```

---

### SEC-19: OVMF_VARS template copied without restrictive permissions [LOW]

**Current state:** `swift-qemu-client/src/lib.rs:34`:
```rust
std::fs::copy(vars_template, &config.ovmf_vars)
```

The OVMF_VARS.fd file is copied with the default permissions of the source file. This writable UEFI variable store contains the guest's Secure Boot configuration, PK/KEK/DB keys, and boot order.

**Risk:** If another process in the pod can write to the runtime directory, it can modify the UEFI variable store to disable Secure Boot or inject boot entries before the VM starts.

**Blast radius:** Guest-level impact only. An attacker with write access to the runtime directory could modify the guest's boot chain but cannot escape to the host via OVMF_VARS modification alone.

**Recommended fix:** Set permissions to `0600` after copy and ensure the runtime directory is only writable by the swiftletd process.

---

## Cross-Cutting Observations

### Trust Boundary Summary

```
User (kubectl) --> Kubernetes API --> Controller (Go)
                                        |
                                        | Creates ConfigMap (RuntimeIntent)
                                        | Creates Pod
                                        v
                                    Launcher Pod
                                      |-- gpu-init (writes sysfs, runs fmpm) [HOST TRUST]
                                      |-- network-init (creates bridge/tap) [HOST TRUST]
                                      |-- swiftletd (spawns hypervisor) [HOST TRUST]
                                            |
                                            | Spawns process
                                            v
                                        CH / QEMU
                                            |
                                            | VFIO passthrough
                                            v
                                        Guest VM [UNTRUSTED]
```

The primary trust boundaries are:
1. **User to Controller**: Validated by Kubernetes RBAC and CRD validation webhooks (not yet implemented for GPU types).
2. **Controller to Pod**: RuntimeIntent ConfigMap is the data boundary. No integrity verification.
3. **Pod to Host**: Privileged containers have no boundary. This is the most critical gap.
4. **Guest to Host**: VFIO/IOMMU provides hardware isolation. This is generally strong but depends on correct IOMMU configuration and no ACS bypass.

### What is NOT a Finding

- **VFIO/IOMMU isolation**: When properly configured with IOMMU groups, VFIO prevents guest DMA outside assigned regions. This is a hardware guarantee and is considered secure.
- **Kubernetes service account in launcher pod**: The default service account has minimal permissions. swiftletd patches its own pod annotations and SwiftGuest status, which requires RBAC. This is correctly scoped.
- **Cloud Hypervisor HTTP API socket**: Bound to a Unix socket inside the pod's emptyDir. Not reachable from outside the pod.
- **QMP socket**: Same -- bound to a Unix socket inside the pod's emptyDir.

---

## Priority Remediation Order

1. **SEC-02**: Harden network-init first (lowest risk, clearest capability set: `NET_ADMIN` + `NET_RAW`). Verify with real cluster.
2. **SEC-01**: Harden launcher container (test non-GPU path first, then kernel boot, then GPU).
3. **SEC-05**: Add PCI address validation regex in the controller.
4. **SEC-04**: Scope /dev/vfio mounts to specific IOMMU groups.
5. **SEC-06**: Validate FM partition GPU indices match allocated GPUs.
6. **SEC-12**: Set `immutable: true` on RuntimeIntent ConfigMaps.
7. **SEC-08/09**: Tighten RBAC verbs and secret scope.
8. **SEC-03**: Harden gpu-init (most complex due to sysfs write requirements).
9. All remaining LOW findings.
