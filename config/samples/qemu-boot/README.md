# QEMU Boot

Boot an Ubuntu Noble guest using the QEMU hypervisor path (OVMF/UEFI).
This is a self-contained manifest — image, seed profile, and guest are in one file.

The `kubeswift.io/hypervisor-override: "qemu"` annotation forces the QEMU path
without requiring GPU hardware.

## Prerequisites

- KubeSwift CRDs and controller deployed
- SwiftGuestClass `default` applied (from `shared/`)
- RBAC applied: `kubectl apply -k config/rbac`

## Apply

```bash
kubectl apply -f config/samples/shared/swiftguestclass-default.yaml
kubectl apply -f config/samples/qemu-boot/swiftguest-qemu.yaml
kubectl get swiftguest qemu-test -w
```

## Expected result

- SwiftImage `ubuntu-noble-qemu`: phase=Ready
- SwiftGuest `qemu-test`: phase=Running, hypervisor=qemu, primaryIP populated

## Cleanup

```bash
kubectl delete swiftguest qemu-test
kubectl delete swiftimage ubuntu-noble-qemu
kubectl delete swiftseedprofile qemu-test-seed
kubectl delete swiftguestclass default
```
