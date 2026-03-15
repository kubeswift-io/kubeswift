# KubeSwift Sample Manifests

Sample manifests for booting a Linux cloud guest with KubeSwift.

All SwiftSeedProfile samples include `ssh_authorized_keys` for SSH access. Replace the key with your own for production use.

## Apply order

Apply resources in this order (dependencies first):

1. **SwiftGuestClass** – defines CPU, memory, root disk size
2. **SwiftImage** – image source (http) and format; controller imports to Ready
3. **SwiftSeedProfile** – cloud-init user-data, meta-data
4. **SwiftGuest** – references image, guest class, and seed profile

The controller creates a pod for each SwiftGuest when SwiftImage is Ready. See `swiftguest-with-pod.yaml` and docs/swiftguest-reconcile.md.

## Usage

```bash
kubectl apply -f config/samples/swiftguestclass-default.yaml
kubectl apply -f config/samples/swiftimage-http.yaml
kubectl apply -f config/samples/swiftseedprofile-minimal.yaml
kubectl apply -f config/samples/swiftguest-sample.yaml
```

## Prerequisites

- KubeSwift controllers and CRDs installed
- swiftletd RBAC for status reporting: `kubectl apply -k config/rbac/ -n <namespace>`
- See [docs/first-boot.md](../docs/first-boot.md), [docs/swiftletd-mvp.md](../docs/swiftletd-mvp.md), and [docs/smoke-verification.md](../docs/smoke-verification.md) for full prerequisites and verification
