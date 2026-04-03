# Rocky Linux 9 Boot

Boot a Rocky Linux 9 cloud image on Cloud Hypervisor. Alternative distro to Ubuntu Noble.

## Prerequisites

- KubeSwift CRDs and controller deployed
- SwiftGuestClass `default` and SwiftSeedProfile `minimal` applied (from `shared/`)
- RBAC applied: `kubectl apply -k config/rbac`

## Apply

```bash
kubectl apply -f config/samples/shared/swiftguestclass-default.yaml
kubectl apply -f config/samples/shared/swiftseedprofile-minimal.yaml
kubectl apply -f config/samples/rocky/swiftimage-rocky9.yaml
kubectl get swiftimage rocky9-cloud -w
# Once Ready:
kubectl apply -f config/samples/rocky/swiftguest-rocky.yaml
kubectl get swiftguest rocky -w
```

## Cleanup

```bash
kubectl delete swiftguest rocky
kubectl delete swiftimage rocky9-cloud
```
