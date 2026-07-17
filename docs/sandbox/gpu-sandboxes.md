# GPU sandboxes

Pass a GPU into a [sandbox](overview.md) — an ephemeral microVM with a real VM
boundary around GPU code (CUDA inference, untrusted or multi-tenant GPU
workloads). Two allocation backends: **native** SwiftGPU (`spec.gpuProfileRef`)
allocates the device(s) at controller time and pins the sandbox to that node;
**DRA** (`spec.gpuResourceClaim`) lets the kube-scheduler and a DRA driver
allocate at pod-schedule time. Either way the sandbox boots firmware-less
(mode-3) with the GPU passed through via VFIO.

> **Status: Phase 1 — single Tier-1 GPU, cluster-validated on a GTX 1080.**
> Multi-GPU / NVLink and multi-node distributed inference are scoped but not yet
> shipped (they need multi-GPU hardware).
>
> **Two allocation backends, mutually exclusive:** `spec.gpuResourceClaim` (DRA —
> the scheduler + a DRA driver allocate at pod-schedule time) and
> `spec.gpuProfileRef` (native SwiftGPU — the SwiftGPU controller allocates a
> `SwiftGPUProfile` against a `SwiftGPUNode` at controller time, stamps
> `status.gpu`, pins the sandbox to that node, and `gpu-init` binds the specific
> BDF). Native suits heterogeneous estates / clusters without the NVIDIA DRA
> stack. Native is **tier: pcie only** — a sandbox boots mode-3 (Cloud
> Hypervisor direct-kernel); HGX tiers need the QEMU disk-boot path (a
> SwiftGuest) and are rejected at allocation.

## How it works

`spec.gpuResourceClaim` names a DRA `ResourceClaim` (or template). The
kube-scheduler allocates the device and the KubeSwift DRA driver injects it (CDI
`GPU_PCI_ADDRESSES`); a `gpu-init` container binds it to `vfio-pci`, and swiftletd
synthesizes the Cloud Hypervisor `--device` from the injected env. The **NVIDIA
driver rides the guest OCI image**, not KubeSwift — the sandbox loads it at start.

Because the driver is a loadable module, a GPU sandbox boots the module-capable
**`gpu-sandbox`** kernel profile (the controller selects it automatically; the base
sandbox kernel is monolithic and can't `insmod`).

## Prerequisites

Common to both backends:

1. **A GPU node that is also a kernel node.** The node needs both
   `kubeswift.io/gpu-node=true` (discovery) and **`kubeswift.io/kernel-node=true`**
   — the kernel artifacts and `/dev/kvm` live on kernel nodes, and a GPU sandbox
   stays pinned there (unlike a GPU SwiftGuest). `vfio-pci` must be loadable on the
   node.
2. **The `gpu-sandbox` SwiftKernel:**
   ```bash
   kubectl apply -f config/samples/sandbox/swiftkernel-gpu-sandbox.yaml
   kubectl get swiftkernel gpu-sandbox -w      # wait for Ready (per-node ORAS pull)
   ```

**DRA backend only** (`spec.gpuResourceClaim`):

3. **A DRA GPU driver** — the KubeSwift reference driver (`gpu.kubeswift.io`,
   deviceclass `kubeswift-vfio-gpu`) or the NVIDIA `k8s-dra-driver-gpu`. The node
   publishes a `ResourceSlice` for its GPU(s).
4. **A `ResourceClaimTemplate`** for the GPU (one device of your GPU deviceclass) —
   see `config/samples/sandbox/` for an example.

**Native backend only** (`spec.gpuProfileRef`):

3. **`gpu-discovery` running** on the GPU node, populating the `SwiftGPUNode`
   inventory (no DRA driver, no ResourceSlice, no ResourceClaim).
4. **A `SwiftGPUProfile`** describing the GPU request (`count`, `tier: pcie`) —
   see `config/samples/sandbox/swiftsandbox-gpu-native.yaml`.

## The guest image (BYO)

A GPU sandbox image must ship the NVIDIA driver built for the `gpu-sandbox` kernel
(Linux 6.6.x) and load it at start:

- **Kernel module** — `nvidia.ko` (+ `nvidia-uvm.ko` for CUDA) built against Linux
  6.6.x. **Pascal (GTX 10-series) and older need the proprietary driver**; Turing
  and newer can use `nvidia-open`. Build it against the kernel source tree with the
  buildroot toolchain (see `build/kernels/sandbox` — the module vermagic must match
  the shipped `gpu-sandbox` kernel).
- **Userspace** — `nvidia-smi` + `libnvidia-ml`, plus your framework (vLLM / TGI /
  PyTorch).
- **Entrypoint** — `insmod` the modules, create the `/dev/nvidia*` nodes (read the
  char-major from `/proc/devices`), then run the workload.

Building the driver into the kernel is not an option (the NVIDIA modules are
out-of-tree); it rides the image. This composes with the "build your own base
layers" story ([#395](https://github.com/kubeswift-io/kubeswift/issues/395)).

## Quickstart

```yaml
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftSandbox
metadata:
  name: gpu-infer
spec:
  image: <your-registry>/cuda-vllm@sha256:...   # ships nvidia.ko + nvidia-smi
  cpu: 4
  memory: 8Gi
  gpuResourceClaim:
    resourceClaimTemplateName: single-vfio-gpu   # your DRA template
    tier: pcie
  # kernelProfileRef defaults to gpu-sandbox when a GPU is requested.
```

```bash
kubectl apply -f config/samples/sandbox/swiftsandbox-gpu.yaml
kubectl get sbox gpu-infer -w
swiftctl sandbox logs gpu-infer         # nvidia-smi output, workload logs
```

The scheduler places the sandbox on a node with a free GPU device; `kubectl get
resourceclaim` shows the allocation.

For the native backend instead of DRA — no ResourceClaim, no scheduler
involvement, the SwiftGPU controller allocates and pins the node — see
[`config/samples/sandbox/swiftsandbox-gpu-native.yaml`](../../config/samples/sandbox/swiftsandbox-gpu-native.yaml).

## Limits

- **Cold-boot only for a plain `SwiftSandbox`.** The webhook rejects `poolRef`
  combined with either `gpuResourceClaim` or `gpuProfileRef` on a user-authored
  sandbox — a warm pool can't cheaply hold a scarce GPU idle for an arbitrary
  claimant. (A [warm GPU pool](#warm-gpu-pools-sub-second-inference-start) holds
  the GPU on the *slot* itself, not the claimant — see below.)
- **Single Tier-1 GPU.** One PCIe GPU per sandbox, no NVLink. Multi-GPU
  tensor-parallel and multi-node are scoped
  ([#390](https://github.com/kubeswift-io/kubeswift/issues/390)) but need hardware.
- **Security.** A GPU sandbox mounts `/dev/vfio` (a wider host surface than the
  default restricted sandbox). GPU inference is often trusted first-party code —
  `network: open` is common — but the VM boundary is the value even so.

## See also

- [Running sandboxes](overview.md)
- [`config/samples/sandbox/`](../../config/samples/sandbox/) — GPU sandbox +
  ResourceClaimTemplate samples
- GPU sandbox scoping / roadmap: [#390](https://github.com/kubeswift-io/kubeswift/issues/390)

## Warm GPU pools (sub-second inference start)

A cold GPU sandbox pays a full boot + driver load + model load per request. For
latency-critical inference, a **warm GPU pool** keeps N pre-booted GPU sandboxes
so a checkout is sub-second (the workload is injected over vsock into an
already-booted, GPU-attached slot).

```yaml
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftSandboxPool
metadata: {name: infer-pool}
spec:
  image: <cuda-inference image>
  cpu: 4
  memory: 8Gi
  gpuProfileRef: {name: infer-gpu}   # makes every warm slot a GPU sandbox
  minWarm: 1
```

Check out a slot with a `SwiftSandbox` that has `poolRef` and **no GPU of its
own** — the GPU comes from the claimed slot:

```yaml
kind: SwiftSandbox
spec:
  image: <same image>
  poolRef: {name: infer-pool}
  command: ["/bin/sh", "-c", "python /app/infer.py"]
```

**The tradeoff is explicit:** each warm slot holds a **whole GPU idle**, so keep
`minWarm` ≤ your free GPU count (on one GPU, `minWarm: 1`). The controller
allocates a GPU per warm slot, gates the slot on it, and releases it when the
slot drains, its checkout completes, or the pool is deleted. **Tier `pcie` only**
(a warm slot boots mode-3 Cloud Hypervisor); HGX tiers are rejected. Full sample:
`config/samples/sandbox/swiftsandboxpool-gpu.yaml`.

## Model preload

A warm GPU slot still pays a cold **model load** on checkout unless the weights
are already there. `spec.model` mounts a model artifact — an OCI image whose
filesystem holds the weights — read-only at `/model` in every warm slot, so a
checkout starts inference without loading anything from a registry or a PVC:

```yaml
apiVersion: sandbox.kubeswift.io/v1alpha1
kind: SwiftSandboxPool
metadata: {name: infer-pool}
spec:
  image: <cuda-inference image>
  cpu: 4
  memory: 8Gi
  gpuProfileRef: {name: infer-gpu}
  model:
    imageRef: <registry>/llama-3-8b@sha256:...   # digest ref preferred
    mountPath: /model                            # default
  minWarm: 1
```

The model is pulled and unpacked **once per node**, keyed by digest, into
`/var/lib/kubeswift/sandbox-models/` and shared into each slot over virtio-fs. So
every slot on the node — and every other pool or cold sandbox using the same
model digest — reads the same host page cache, at no extra memory or disk cost.
`spec.model` works on a plain `SwiftSandbox` too, not just a pool.

It is read-only on both sides (the launcher mounts the cache RO and the guest
mounts it `-o ro`), so one checkout can't corrupt the model other slots share.

**Packaging a model.** Any OCI image whose filesystem is the weights:

```dockerfile
FROM scratch
COPY model/ /
```

```bash
docker build -t <registry>/llama-3-8b:v1 . && docker push <registry>/llama-3-8b:v1
```

Push it to any registry — the same one your images already come from. Set
`spec.verifyKeySecretRef` and the model is cosign-verified before it is
materialized, exactly like the rootfs (same key; the model is expected to come
from the same trust domain). `spec.imagePullSecret` covers a private model
registry.

**Why an OCI artifact and not a shared PVC.** The registry already distributes
every other KubeSwift artifact (images, kernels, VM disks), so a model rides the
same path: content-addressed and digest-pinned, signable, portable across
clusters with no per-cluster PVC to pre-populate, deduplicated per node for free,
and with no RWX storage class to depend on. If your weights already live on a
shared filesystem — or are too large to push to a registry — mount that PVC
read-only via `spec.scratchDisk.pvcRef` instead; that path is unchanged.

The first pull on a node costs what the bytes cost; every slot and pool after
that is a cache hit. Pre-pull a node by running one throwaway sandbox with the
same `spec.model`.
