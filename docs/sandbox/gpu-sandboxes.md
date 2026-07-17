# GPU sandboxes

Pass a GPU into a [sandbox](overview.md) — an ephemeral microVM with a real VM
boundary around GPU code (CUDA inference, untrusted or multi-tenant GPU
workloads). The GPU is allocated through Kubernetes **DRA** (Dynamic Resource
Allocation): the scheduler picks the node and device, and the sandbox boots
firmware-less (mode-3) with the GPU passed through via VFIO.

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

1. **A DRA GPU driver** — the KubeSwift reference driver (`gpu.kubeswift.io`,
   deviceclass `kubeswift-vfio-gpu`) or the NVIDIA `k8s-dra-driver-gpu`. The node
   publishes a `ResourceSlice` for its GPU(s).
2. **A GPU node that is also a kernel node.** The node needs both
   `kubeswift.io/gpu-node=true` (discovery) and **`kubeswift.io/kernel-node=true`**
   — the kernel artifacts and `/dev/kvm` live on kernel nodes, and a GPU sandbox
   stays pinned there (unlike a GPU SwiftGuest). `vfio-pci` must be loadable on the
   node.
3. **The `gpu-sandbox` SwiftKernel:**
   ```bash
   kubectl apply -f config/samples/sandbox/swiftkernel-gpu-sandbox.yaml
   kubectl get swiftkernel gpu-sandbox -w      # wait for Ready (per-node ORAS pull)
   ```
4. **A `ResourceClaimTemplate`** for the GPU (one device of your GPU deviceclass) —
   see `config/samples/sandbox/` for an example.

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

## Limits (Phase 1)

- **DRA only.** `gpuResourceClaim` (not `gpuProfileRef`). Needs a DRA driver on the
  cluster.
- **Cold-boot only.** `gpuResourceClaim` and `poolRef` are mutually exclusive — a
  warm pool can't hold a scarce GPU idle. (Warm GPU pools are a later tier.)
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

**Model preload** (a pool-shared read-only model cache mounted into every warm
slot, so the weights are resident before checkout) is the next step; today, bake
the model into the image or mount it via `spec.scratchDisk` on the pool slots.
