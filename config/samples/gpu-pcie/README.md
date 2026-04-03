# GPU Passthrough — Tier 1 PCIe

Single A100-PCIe GPU passthrough using Cloud Hypervisor.

## Prerequisites

- KubeSwift CRDs and controller deployed
- A node with a PCIe GPU, labeled: `kubectl label node <name> kubeswift.io/gpu-node=true`
- GPU Discovery DaemonSet deployed and SwiftGPUNode showing the GPU
- RBAC applied: `kubectl apply -f config/manager/controller-manager-rbac.yaml`
- SwiftImage `ubuntu-noble-qemu` Ready (create via `qemu-boot/` scenario first)

## Apply

```bash
kubectl apply -f config/samples/shared/swiftguestclass-default.yaml
kubectl apply -f config/samples/shared/swiftseedprofile-minimal.yaml
kubectl apply -f config/samples/gpu-pcie/swiftgpuprofile-a100-pcie.yaml
kubectl apply -f config/samples/gpu-pcie/swiftguest-gpu.yaml
kubectl get swiftguest gpu-test -w
```

## Expected result

- SwiftGuest `gpu-test`: GPUAllocated=True, phase=Running
- Inside guest: `nvidia-smi` shows the GPU

## Cleanup

```bash
kubectl delete swiftguest gpu-test
kubectl delete swiftgpuprofile a100-pcie-single
```
