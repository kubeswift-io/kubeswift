# GPU Passthrough — DRA Allocation Backend

GPU passthrough where the **kube-scheduler** allocates the GPU through a
`ResourceClaim` (Kubernetes Dynamic Resource Allocation), instead of the
native SwiftGPU controller picking node + devices. The VM runtime path is
identical — VFIO whole-GPU passthrough into Cloud Hypervisor (or QEMU for
`hgx-*` tiers).

Full operator guide: [docs/gpu/dra-allocation.md](../../../docs/gpu/dra-allocation.md).

## Files

| File | What it shows |
|------|---------------|
| `resourceclaimtemplate-single-gpu.yaml` | One GPU per guest, claim minted per pod (recommended) |
| `swiftguest-dra-gpu.yaml` | SwiftGuest with `gpuResourceClaim.resourceClaimTemplateName` (the cluster-validated shape) |
| `resourceclaim-shared.yaml` | Pre-created claim — the reservation outlives the pod |
| `swiftguest-dra-shared-claim.yaml` | SwiftGuest with `gpuResourceClaim.resourceClaimName` |
| `resourceclaimtemplate-selectors.yaml` | CEL device selectors (GPU model, vfio-ready nodes only) |

## Prerequisites

- Kubernetes **>= 1.34** (`resource.k8s.io/v1` GA)
- **CDI enabled** in containerd on GPU nodes (off by default in containerd 1.7 —
  see the [node prep section](../../../docs/gpu/dra-allocation.md#prerequisites)
  for the k0s drop-in)
- GPU node labeled `kubeswift.io/gpu-node=true`, IOMMU on, `vfio-pci` module loaded
- The reference DRA driver + DeviceClass installed:

```bash
kubectl apply -f config/dra-driver/dra-driver.yaml    # DaemonSet + RBAC
kubectl apply -f config/dra-driver/deviceclass.yaml   # DeviceClass kubeswift-vfio-gpu
kubectl get resourceslice                             # one slice per GPU node
```

- SwiftImage `ubuntu-noble` Ready (create via `disk-boot/` scenario first)

## Apply

```bash
kubectl apply -f config/samples/shared/swiftguestclass-default.yaml
kubectl apply -f config/samples/shared/swiftseedprofile-minimal.yaml
kubectl apply -f config/samples/dra-gpu/resourceclaimtemplate-single-gpu.yaml
kubectl apply -f config/samples/dra-gpu/swiftguest-dra-gpu.yaml
kubectl get swiftguest dra-gpu-vm -w
```

## Expected result

```
$ kubectl get resourceclaim
NAME                   STATE                AGE
dra-gpu-vm-gpu-vkt4l   allocated,reserved   1m

$ kubectl get swiftguest dra-gpu-vm
NAME         PHASE     NODE   IP              AGE
dra-gpu-vm   Running   boba   192.168.99.13   2m

$ kubectl get swiftguest dra-gpu-vm -o jsonpath='{.status.gpu}'
{"devices":["0000:01:00.0"],"hypervisor":"cloud-hypervisor","nodeName":"boba","partitionId":-1}
```

- Condition `GPUAllocated=True` (`allocated 1 GPU(s) on node boba`); while the
  scheduler is still deciding, the guest shows `GPUClaimPending` instead.
- Inside the guest: `lspci | grep -i nvidia` shows the GPU.

No `SwiftGPUProfile` and no `SwiftGPUNode` are involved — allocation state
lives in the `ResourceClaim`.

## Cleanup

```bash
kubectl delete swiftguest dra-gpu-vm
# per-pod claims are garbage-collected with the pod; shared claims are not:
kubectl delete swiftguest dra-gpu-vm-reserved --ignore-not-found
kubectl delete resourceclaim reserved-vfio-gpu --ignore-not-found
```

## Limitations (v1)

- Whole-GPU `tier: pcie` passthrough is the cluster-validated path; `hgx-*`
  tiers under DRA are untested (no HGX hardware).
- DRA guests are **excluded from automated offline GPU migration on drain**
  (that choreography is native-backend-only); they block a drain like other
  VFIO guests.
- No MIG/fractional, no multi-node NVLink/IMEX yet (later, hardware-gated phases).
