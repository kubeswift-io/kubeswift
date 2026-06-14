# GPU Allocation via DRA (Dynamic Resource Allocation)

KubeSwift has two GPU allocation backends. The **native** backend
(`spec.gpuProfileRef`) is the default: the SwiftGPU controller inventories GPUs
through the discovery DaemonSet and picks node + devices itself. The **DRA**
backend (`spec.gpuResourceClaim`) delegates that decision to Kubernetes: the
launcher pod carries a `ResourceClaim`, and the **kube-scheduler** allocates a
GPU from the `ResourceSlice`s a DRA driver publishes — the standard
`resource.k8s.io/v1` API (GA since Kubernetes 1.34).

Either way, the VM runtime path is identical: the GPU is VFIO-passed whole into
the guest by Cloud Hypervisor (`tier: pcie`) or QEMU (`tier: hgx-*`). DRA only
changes *who decides which GPU on which node*.

This guide covers the operator workflow with KubeSwift's **reference DRA
driver** (`kubeswift-dra-driver`, driver name `gpu.kubeswift.io`). The design,
rationale, and cluster-validation evidence live in
[docs/design/dra-gpu-integration.md](../design/dra-gpu-integration.md).

## Choosing a backend

| | Native (`gpuProfileRef`) | DRA (`gpuResourceClaim`) |
|---|---|---|
| Who allocates | SwiftGPU controller (`SwiftGPUNode` inventory) | kube-scheduler (`ResourceClaim` + `ResourceSlice`) |
| Allocation API | KubeSwift CRDs (`SwiftGPUProfile`/`SwiftGPUNode`) | Standard Kubernetes `resource.k8s.io/v1` |
| Requires | gpu-discovery DaemonSet | a DRA driver + CDI enabled in containerd |
| Device selection | profile `model`/`tier`/NUMA matching | `DeviceClass` + CEL selectors on device attributes |
| GPU sharing across the cluster's stack | KubeSwift-only view | claims/slices visible to any DRA-aware component |
| Drain auto-migration (offline GPU migration) | yes (reserve-and-reallocate) | **no** — DRA guests block a drain like other VFIO guests |
| HGX tiers (2/3), Fabric Manager partitions | yes (validated on Tier 1; HGX hardware-gated) | untested under DRA |
| MIG / fractional, multi-node NVLink (IMEX) | not expressible | future, hardware-gated phases |
| Maturity | the long-validated default | cluster-validated for whole-GPU `tier: pcie` |

Pick exactly one per guest — `gpuProfileRef` and `gpuResourceClaim` are
mutually exclusive (webhook-enforced). Both backends can coexist in one
cluster, even on the same node; a GPU allocated by one is simply busy from the
other's perspective only if something tracks it — **do not point both backends
at the same physical GPUs** unless you partition them (e.g. dedicate nodes to
one backend).

## How it works

```
SwiftGuest (spec.gpuResourceClaim)
   │ SwiftGPU controller: GPUClaimPending=True (no node, no device yet)
   ▼
launcher pod with pod.spec.resourceClaims  ──►  kube-scheduler picks node + GPU
   │                                            (from the driver's ResourceSlice)
   ▼
kubelet calls the DRA driver (NodePrepareResources)
   │ driver writes a CDI spec: inject GPU_PCI_ADDRESSES=<bdf> into the
   │ containers referencing the claim
   ▼
gpu-init (unchanged) binds the BDF to vfio-pci
   ▼
swiftletd reads GPU_PCI_ADDRESSES (intent gpu.deviceSource: env)
   ▼
cloud-hypervisor --device path=/sys/bus/pci/devices/<bdf>/
   │
   └─ controller reads the allocation back from the claim → status.gpu,
      GPUAllocated=True
```

Key property: the device identity travels through **CDI container edits**, not
a controller round-trip — there is no race between allocation and the gpu-init
init container, and `kubectl get` status is purely observational.

## Prerequisites

1. **Kubernetes >= 1.34** (`resource.k8s.io/v1` is GA there).
2. **CDI enabled in containerd** on every GPU node. containerd 1.7 ships with
   `enable_cdi = false`. On k0s, add an import drop-in and restart the worker
   (running guests survive the restart):

   ```toml
   # /etc/k0s/containerd.d/99-kubeswift-cdi.toml
   [plugins."io.containerd.grpc.v1.cri"]
     enable_cdi = true
     cdi_spec_dirs = ["/etc/cdi", "/var/run/cdi"]
   ```

   ```bash
   sudo systemctl restart k0sworker
   ```

   On other distributions, set the same keys in the containerd CRI plugin
   config. containerd 2.x enables CDI by default.
3. **GPU node prep** — the same host requirements as the native backend
   ([gpu-passthrough.md](../gpu-passthrough.md)): IOMMU on, `vfio-pci` module
   loaded persistently, node labeled `kubeswift.io/gpu-node=true`.
4. A **DRA driver**. The rest of this guide uses KubeSwift's reference driver;
   adapting an external driver (e.g. NVIDIA's `k8s-dra-driver-gpu`) is a
   hardware-gated follow-up — see [External drivers](#external-dra-drivers).

## Install the reference driver

**Helm (recommended).** Enable the `dra` toggle (independent of `gpuDiscovery` —
the DRA driver does its own discovery, so a DRA-only cluster keeps
`gpuDiscovery.enabled=false`):

```bash
helm upgrade --install kubeswift oci://ghcr.io/projectbeskar/charts/kubeswift \
  --version 0.4.1 -n kubeswift-system --create-namespace \
  --set dra.enabled=true
```

This deploys the DaemonSet + RBAC and creates the `kubeswift-vfio-gpu`
DeviceClass (set `--set dra.deviceClass.create=false` if you manage it
out-of-band).

**Kustomize / manual.** Only for non-Helm installs. The standalone manifest's
image is **pinned to the release** (the registry publishes `vX.Y.Z` tags only —
there is no `:latest`), so check out the matching release tag before applying.

```bash
kubectl apply -f config/dra-driver/dra-driver.yaml    # DaemonSet + RBAC (kubeswift-system)
kubectl apply -f config/dra-driver/deviceclass.yaml   # DeviceClass: kubeswift-vfio-gpu
```

> **Do NOT apply these over a Helm install with `dra.enabled=true`** — the chart
> already deploys the driver with a version-managed image, and re-applying the
> standalone manifest would overwrite that image with the manifest's pinned tag.
> Use one path or the other, not both.

The driver runs on `kubeswift.io/gpu-node=true` nodes, discovers GPUs from
sysfs (no NVIDIA userspace needed), and publishes one `ResourceSlice` per node:

```
$ kubectl get resourceslice
NAME                          NODE   DRIVER             POOL   AGE
boba-gpu.kubeswift.io-mg7pp   boba   gpu.kubeswift.io   boba   18m
```

Each device carries the attributes `pciAddress`, `vendorDevice` (PCI
vendor:device, e.g. `10de:1b80`), `numaNode`, `iommuGroup`, and `vfioReady`
(whether the node has the vfio-pci driver loaded). Device names encode the PCI
address (`gpu-0000-01-00-0` ⇄ `0000:01:00.0`) so an allocation is identifiable
even without the device-status feature.

The driver only advertises devices and hands identity over — **vfio-pci binding
stays with the launcher pod's gpu-init**, same as the native backend.

## Boot a GPU guest

Create a `ResourceClaimTemplate` (one GPU per guest; each guest gets its own
claim, garbage-collected with the pod):

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: single-vfio-gpu
spec:
  spec:
    devices:
      requests:
        - name: gpu # matches gpuResourceClaim's default requestName
          exactly:
            deviceClassName: kubeswift-vfio-gpu
            count: 1
```

Reference it from a SwiftGuest — note there is **no SwiftGPUProfile and no
nodeName**; the scheduler picks the node:

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: dra-gpu-vm
spec:
  imageRef:
    name: ubuntu-noble
  gpuResourceClaim:
    resourceClaimTemplateName: single-vfio-gpu
    tier: pcie # pcie -> Cloud Hypervisor (default); hgx-* -> QEMU
  guestClassRef:
    name: default
  seedProfileRef:
    name: minimal
  runPolicy: Running
```

Ready-to-apply versions of these (plus the variants below) are in
[config/samples/dra-gpu/](../../config/samples/dra-gpu/).

Verify:

```
$ kubectl get resourceclaim
NAME                   STATE                AGE
dra-gpu-vm-gpu-vkt4l   allocated,reserved   1m

$ kubectl get swiftguest dra-gpu-vm
NAME         PHASE     NODE   IP              AGE
dra-gpu-vm   Running   boba   192.168.99.13   2m

$ kubectl get swiftguest dra-gpu-vm -o jsonpath='{.status.gpu}'
{"devices":["0000:01:00.0"],"hypervisor":"cloud-hypervisor","nodeName":"boba","partitionId":-1}

$ swiftctl ssh dra-gpu-vm -- lspci | grep -i nvidia
01:00.0 VGA compatible controller: NVIDIA Corporation GP104 [GeForce GTX 1080]
```

## Pre-created (shared) claims

A standalone `ResourceClaim` referenced via `resourceClaimName` holds its
allocation independently of any pod — the guest reuses the **same physical
GPU** across stop/start and pod recreation:

```yaml
spec:
  gpuResourceClaim:
    resourceClaimName: reserved-vfio-gpu
    tier: pcie
```

**Constraint:** DRA itself lets many pods share a claim, but a VFIO-passed GPU
can back only one running VM. Keep a 1:1 claim-to-guest mapping; never point
two running SwiftGuests at the same shared claim.

## Selecting devices with CEL

Requests can constrain which devices qualify, using the driver's published
attributes (qualified by the driver name in CEL):

```yaml
requests:
  - name: gpu
    exactly:
      deviceClassName: kubeswift-vfio-gpu
      count: 1
      selectors:
        - cel:
            expression: >-
              device.attributes["gpu.kubeswift.io"].vendorDevice == "10de:1b80" &&
              device.attributes["gpu.kubeswift.io"].vfioReady == true
```

The `vfioReady` guard is recommended in mixed fleets: it keeps the scheduler
from placing a GPU guest on a node where the vfio-pci module is missing (where
gpu-init's bind would fail after scheduling).

## `spec.gpuResourceClaim` reference

| Field | Meaning |
|---|---|
| `resourceClaimTemplateName` | Mint a per-pod claim from this template (recommended). Mutually exclusive with `resourceClaimName`. |
| `resourceClaimName` | Use a pre-created shared claim (reservation outlives the pod). |
| `requestName` | Device-request name inside the claim to read the allocation from. Default `"gpu"`. |
| `tier` | Hypervisor selection, same semantics as `SwiftGPUProfile.tier`: `pcie` (default, Cloud Hypervisor), `hgx-shared`/`hgx-full` (QEMU). |
| `hugepages` | GPU memory hugepage backing (`1Gi`, `2Mi`, or empty). |

Because there is no profile, these are the only VM-shape knobs; everything
else (which device, which node) is the claim's job. `gpuResourceClaim` is
mutually exclusive with `gpuProfileRef`, `kernelRef`, `cloneFromSnapshot`, and
`osType: windows` — the same constraint family as `gpuProfileRef`.

## Conditions and status

- **`GPUClaimPending=True`** — the guest is DRA-backed and the scheduler has
  not allocated yet (or the controller hasn't observed it). The launcher pod
  exists and is what the scheduler is placing. Cleared once resolved.
- **`GPUAllocated=True`** (`allocated 1 GPU(s) on node boba`) — the allocation
  was read back from the claim into `status.gpu`
  (`devices`, `nodeName`, `hypervisor`, `partitionId: -1`).
- `kubectl get resourceclaim` shows the authoritative allocation state
  (`allocated,reserved`); `kubectl get swiftgpunode` plays **no role** for
  DRA-backed guests.

## Limitations (v1)

- **Whole-GPU `tier: pcie` is the validated path.** `hgx-shared`/`hgx-full`
  under DRA are untested (no HGX hardware); Fabric Manager partition selection
  is native-only.
- **No automated drain evacuation.** The offline GPU migration choreography
  (reserve-on-target-before-stop) is native-backend-only. A DRA GPU guest
  blocks `kubectl drain` and needs manual handling, like other VFIO guests
  without that support.
- **No MIG/fractional, no multi-node NVLink (IMEX)** — these are precisely the
  capabilities DRA will eventually unlock, but they need datacenter hardware
  and are tracked as later phases in the design doc.
- NUMA placement inside the guest is not derived from the claim in v1
  (single-node topology; `numaNode` attribute is informational).

## Troubleshooting

**Guest stuck with `GPUClaimPending=True`, claim `pending`.** The scheduler
found no allocatable device. Check `kubectl get resourceslice` (does a slice
exist for a GPU node? is the driver pod running?), and your CEL selectors
(e.g. `vfioReady == true` excludes nodes without the vfio-pci module —
`modprobe vfio-pci` on the node, the next driver republish updates the
attribute).

**Launcher pod Running but the guest never gets an IP / `GuestRunning` stays
false; swiftletd log shows:**

```
gpu.deviceSource=env but GPU_PCI_ADDRESSES is empty — CDI injection from the
DRA driver did not happen (is enable_cdi on, and does the container reference
the claim?)
```

CDI is not enabled in containerd on that node (the prerequisite drop-in +
worker restart), or the CDI spec write failed (check the driver pod's logs).
This is fail-loud by design — the VM is never booted GPU-less. Judge DRA GPU
guests by `GuestRunning`/IP, not by the pod phase.

**Driver pod CrashLoopBackOff with `bind: no such file or directory` on
`/var/lib/kubelet/plugins/gpu.kubeswift.io/dra.sock`.** You are running a
driver image older than the fix that creates its plugin directory — upgrade
the DaemonSet image.

**Webhook rejection on create.** `gpuProfileRef` and `gpuResourceClaim` are
mutually exclusive, exactly one of `resourceClaimName`/
`resourceClaimTemplateName` must be set, and the same boot-source rules as the
native backend apply (disk boot only).

**`strict decoding error: unknown field "spec.gpuResourceClaim"`.** The
cluster's SwiftGuest CRD predates the field — re-apply the CRDs
(`kubectl apply -f config/crd/bases/` or `make deploy`; Helm upgrades do not
touch CRDs).

## External DRA drivers

The reference driver exists to validate the pipeline and serve clusters that
want VFIO GPU allocation without the NVIDIA software stack. Using NVIDIA's
`k8s-dra-driver-gpu` instead requires pinning its device-status/CDI schema —
isolated to a single adapter function in KubeSwift's claim read-back — and
deciding binding ownership (the reference driver leaves vfio binding to
gpu-init). That work is hardware-gated; status is tracked in
[the design doc](../design/dra-gpu-integration.md) (§A6/§A8).

## See also

- [GPU Passthrough (native backend)](../gpu-passthrough.md) — tiers, discovery
  DaemonSet, SwiftGPUProfile reference, Fabric Manager
- [Samples](../../config/samples/dra-gpu/) — apply-ready manifests for
  everything above
- [Design doc + cluster-validation evidence](../design/dra-gpu-integration.md)
