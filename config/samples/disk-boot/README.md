# Disk Boot (Cloud Hypervisor)

Boot an Ubuntu Noble 24.04 cloud image on Cloud Hypervisor with CLOUDHV.fd UEFI firmware.

## Prerequisites

- KubeSwift CRDs and controller deployed
- RBAC applied: `kubectl apply -k config/rbac`

## Apply

```bash
kubectl apply -f config/samples/shared/swiftguestclass-default.yaml
kubectl apply -f config/samples/disk-boot/swiftimage-ubuntu-noble.yaml
kubectl apply -f config/samples/shared/swiftseedprofile-minimal.yaml
# Wait for image import (5-15 minutes)
kubectl get swiftimage ubuntu-noble -w
# Once Ready:
kubectl apply -f config/samples/disk-boot/swiftguest-sample.yaml
kubectl get swiftguest sample -w
```

## Expected result

- SwiftImage `ubuntu-noble`: phase=Ready
- SwiftGuest `sample`: phase=Running, GuestRunning=True, primaryIP populated
- Hypervisor: cloud-hypervisor

## Cleanup

```bash
kubectl delete swiftguest sample
kubectl delete swiftimage ubuntu-noble
kubectl delete swiftseedprofile minimal
kubectl delete swiftguestclass default
```
