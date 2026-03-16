# KubeSwift Smoke Test: Operator Checklist (Ubuntu x86_64)

Host-level prerequisites for running the current smoke-test implementation on Ubuntu x86_64 machines. Derived from `github.com/projectbeskar/kubeswift` codebase and Cloud Hypervisor requirements.

---

## 1. Kernel Features

| Item | Requirement | Verify |
|------|-------------|--------|
| KVM support | Kernel modules `kvm`, `kvm_intel` (Intel) or `kvm_amd` (AMD) loaded | `lsmod \| grep kvm` |
| Minimum kernel | 4.11+ (KVM); 5.6+ recommended for performance | `uname -r` |
| Hardware virtualization | Intel VT-x or AMD-V enabled in BIOS | `egrep -c '(vmx\|svm)' /proc/cpuinfo` > 0 |
| `/dev/kvm` | Device node present and accessible | `ls -la /dev/kvm`; `test -r /dev/kvm` |

---

## 2. Packages

| Package | Purpose | Install |
|---------|---------|---------|
| `kvm` (or `qemu-kvm`) | KVM kernel modules and user-space tools | `apt install kvm` or `apt install qemu-kvm` |
| `kubectl` | Smoke test script | `apt install kubectl` or install via other means |

**Note:** Cloud Hypervisor is bundled in the swiftletd container image; no host install required. The smoke test does not require `cloud-hypervisor` or `qemu-img` on the host.

---

## 3. Services

| Service | Requirement | Verify |
|---------|-------------|--------|
| `kvm` modules | Loaded at boot | `lsmod \| grep kvm`; add to `/etc/modules` if needed |
| No dedicated KubeSwift service | Controllers run in-cluster; swiftletd runs per-pod | — |

---

## 4. Container Runtime Settings

| Setting | Requirement | Notes |
|---------|-------------|-------|
| `/dev/kvm` access | Guest pods must have `/dev/kvm` device | **Current pod spec does not add this**; add `securityContext.devices` or equivalent |
| Nested virtualization | Not required for bare-metal; required if KubeSwift runs inside VMs | For nested: enable `nested` on host KVM; container runtime must expose `/dev/kvm` |
| Device passthrough | `/dev/kvm` must be passed into the launcher container | CRI/containerd: ensure device cgroup allows `/dev/kvm` |

**Concrete fix for current implementation:** The SwiftGuest pod spec must add:

1. **Device passthrough** for `/dev/kvm` (e.g. `hostPath` volume or CRI device configuration)
2. **Capabilities** – `SYS_ADMIN` or `privileged: true` (typical for KVM in containers)

Example addition to pod spec:

```yaml
spec:
  containers:
    - name: launcher
      securityContext:
        privileged: true  # or capabilities: { add: ["SYS_ADMIN"] }
      volumeMounts:
        # ... existing mounts ...
        - name: dev-kvm
          mountPath: /dev/kvm
  volumes:
    # ... existing volumes ...
    - name: dev-kvm
      hostPath:
        path: /dev/kvm
        type: CharDevice
```

---

## 5. Kubernetes Settings

| Setting | Requirement | Notes |
|---------|-------------|-------|
| CRDs | `swiftguests`, `swiftguestclasses`, `swiftimages`, `swiftseedprofiles` | Apply `config/crd/` |
| Controllers | SwiftImage, SwiftGuest (and dependencies) running | Deploy KubeSwift controllers |
| RBAC | `config/rbac/` applied in namespace where SwiftGuests run | Required for swiftletd status patching |
| Storage | Default StorageClass for PVCs; ≥10Gi for import + root disk | SwiftImage import Job and SwiftGuest root-disk use PVCs |
| Node resources | At least one node with ≥2 CPU, ≥2Gi memory for default sample | SwiftGuestClass default: 2 CPU, 2Gi |
| Feature gates | None specified in codebase | Standard cluster |

---

## 6. Host Paths, Mounts, Privileges

### Paths used by swiftletd (inside pod)

| Path | Source | Purpose |
|------|--------|---------|
| `/var/lib/kubeswift/run` | `emptyDir` | Per-guest runtime dir, CH socket, seed output |
| `/var/lib/kubeswift/disks/root` | PVC | Root disk image |
| `/var/lib/kubeswift/intent` | ConfigMap | `runtime-intent.json` |
| `/var/lib/kubeswift/seed` | ConfigMap (optional) | NoCloud seed when `seedProfileRef` present |

### Host-level requirements

| Item | Requirement | Notes |
|------|-------------|-------|
| `/dev/kvm` | Must be available to launcher container | **Not in current pod spec**; add device/volume |
| Host paths | None | All paths are in-pod (emptyDir, PVC, ConfigMap) |
| Privileges | KVM access implies `SYS_ADMIN` or `privileged` in many setups | Documented in Cloud Hypervisor container usage |

---

## 7. Assumptions and Gaps (Not Yet Documented)

| Assumption / Gap | Impact |
|------------------|--------|
| **`/dev/kvm` not in pod spec** | Cloud Hypervisor will fail without KVM; current spec cannot run VMs on bare metal |
| **SwiftImage sample format mismatch** | `swiftimage-http.yaml` uses `format: raw` but `noble-server-cloudimg-amd64.img` is QCow2; StubConverter errors on qcow2→raw. Use `format: qcow2` and ensure Prepare passes through, or use a raw image URL |
| **NoCloud seed as directory** | swiftletd passes NoCloud directory path to CH; CH expects ISO/vfat. May fail on some CH builds; doc suggests adding ISO generation |
| **Runtime intent disk path** | Intent uses `rootDisk.path` = `/var/lib/kubeswift/disks/root` (mount dir); import writes `image.raw`. CH expects a file path; if CH requires the full path (e.g. `.../disks/root/image.raw`), the intent may need adjustment |
| **No nodeSelector/tolerations** | Guest pods can schedule on any node; no explicit "KVM-capable" node targeting |
| **Cloud Hypervisor v51.1** | Containerfile pins CH v51.1; CLI format must match |
| **In-cluster Kubeconfig** | swiftletd assumes in-cluster config (service account) for status patching |
| **Single launcher container** | No init container; swiftletd does all setup |
| **Import Job uses curl** | No retries or checksum validation; assumes URL returns valid image |
| **PVC 10Gi default** | Import PVC requests 10Gi; Ubuntu cloud image ~600Mi; sufficient for default sample |

---

## Preflight Script

Before joining a worker node or running the smoke test, run the preflight script to validate host prerequisites:

```bash
./scripts/kubeswift-preflight.sh
# or: make preflight
```

See [docs/worker-node-preflight.md](worker-node-preflight.md) for download instructions, result interpretation, and exit codes.

---

## Quick Verification Commands

```bash
# Kernel and KVM
uname -r
lsmod | grep kvm
ls -la /dev/kvm
egrep -c '(vmx|svm)' /proc/cpuinfo

# Cluster
kubectl get crd swiftguests.swift.kubeswift.io swiftguestclasses.swift.kubeswift.io swiftimages.image.kubeswift.io swiftseedprofiles.seed.kubeswift.io
kubectl get pods -A | grep -E "kubeswift|controller"
kubectl get sc

# After smoke test
kubectl get swiftimage ubuntu-cloud -o jsonpath='{.status.phase}'
kubectl get swiftguest sample -o jsonpath='{.status.phase}'
kubectl logs -l swift.kubeswift.io/guest=sample -c launcher --tail=50
```

---

## Summary Checklist

- [ ] Run `./scripts/kubeswift-preflight.sh` (or `make preflight`) and resolve any FAIL
- [ ] Kernel 5.6+ (or 4.11+ minimum)
- [ ] KVM modules loaded, `/dev/kvm` present
- [ ] Hardware virtualization enabled (VT-x/AMD-V)
- [ ] `kvm` or `qemu-kvm` package installed
- [ ] Container runtime exposes `/dev/kvm` to pods (pod spec must be updated)
- [ ] Kubernetes CRDs and controllers deployed
- [ ] RBAC applied in target namespace
- [ ] StorageClass with ≥10Gi capacity
- [ ] swiftletd image available on nodes
- [ ] SwiftImage URL accessible from cluster; use `format: qcow2` for Ubuntu cloud `.img` or a raw image URL
