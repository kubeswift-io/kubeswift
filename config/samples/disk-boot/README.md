# Disk Boot (Cloud Hypervisor)

Boot an Ubuntu Focal cloud image on Cloud Hypervisor with cloud-init.

## Prerequisites

- KubeSwift CRDs and controller deployed
- RBAC applied: `kubectl apply -k config/rbac`

## Apply

```bash
kubectl apply -f config/samples/shared/swiftguestclass-default.yaml
kubectl apply -f config/samples/disk-boot/swiftimage-ubuntu-focal.yaml
kubectl apply -f config/samples/shared/swiftseedprofile-minimal.yaml
# Wait for image import (5-15 minutes)
kubectl get swiftimage ubuntu-cloud -w
# Once Ready:
kubectl apply -f config/samples/disk-boot/swiftguest-sample.yaml
kubectl get swiftguest sample -w
```

## Expected result

- SwiftImage `ubuntu-cloud`: phase=Ready
- SwiftGuest `sample`: phase=Running, GuestRunning=True, primaryIP populated
- Hypervisor: cloud-hypervisor

## Cleanup

```bash
kubectl delete swiftguest sample
kubectl delete swiftimage ubuntu-cloud
kubectl delete swiftseedprofile minimal
kubectl delete swiftguestclass default
```
