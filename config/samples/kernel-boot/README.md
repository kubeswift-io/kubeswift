# Kernel Boot (Cloud Hypervisor)

Boot a faas-minimal microVM directly from bzImage + initramfs via SwiftKernel.

## Prerequisites

- KubeSwift CRDs and controller deployed
- At least one node labeled: `kubectl label node <name> kubeswift.io/kernel-node=true`
- RBAC applied: `kubectl apply -k config/rbac`

## Apply

```bash
kubectl apply -f config/samples/shared/swiftguestclass-default.yaml
kubectl apply -f config/samples/kernel-boot/swiftkernel-faas.yaml
# Wait for kernel pull
kubectl get swiftkernel faas-minimal -w
# Once Ready:
kubectl apply -f config/samples/kernel-boot/swiftguest-faas.yaml
kubectl get swiftguest faas-test -w
```

## Expected result

- SwiftKernel `faas-minimal`: phase=Ready
- SwiftGuest `faas-test`: phase=Running, GuestRunning=True, primaryIP populated
- Hypervisor: cloud-hypervisor

## Cleanup

```bash
kubectl delete swiftguest faas-test
kubectl delete swiftkernel faas-minimal
kubectl delete swiftguestclass default
```
