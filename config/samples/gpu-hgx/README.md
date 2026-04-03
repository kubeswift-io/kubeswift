# GPU Passthrough — Tier 2 HGX SXM (Shared NVSwitch)

4x H200-SXM GPU passthrough using QEMU with PCIe root ports and Fabric Manager.

## Prerequisites

- KubeSwift CRDs and controller deployed
- An HGX node with H200-SXM GPUs, labeled: `kubectl label node <name> kubeswift.io/gpu-node=true`
- GPU Discovery DaemonSet deployed and SwiftGPUNode showing GPUs + NVSwitches + FM
- Fabric Manager running on host with matching guest driver version
- RBAC applied: `kubectl apply -f config/manager/controller-manager-rbac.yaml`

## Apply

```bash
kubectl apply -f config/samples/gpu-hgx/swiftgpuprofile-h200-hgx.yaml
# Then create a SwiftGuest referencing this profile (adapt gpu-pcie/swiftguest-gpu.yaml)
```

## Notes

- This profile requires 1Gi hugepages, NUMA topology, and vCPU pinning
- QEMU is selected automatically based on tier=hgx-shared
- See docs/gpu-passthrough.md for full HGX setup guide
