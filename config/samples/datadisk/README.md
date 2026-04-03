# Data Disk Attachment

Attach a secondary data disk to a SwiftGuest. The disk appears as `/dev/vdb` inside the guest.

## Prerequisites

- KubeSwift CRDs and controller deployed
- SwiftGuestClass `default` and SwiftSeedProfile `minimal` applied (from `shared/`)
- SwiftImage `ubuntu-noble` Ready (from `disk-boot/`)
- A data disk SwiftImage — replace the placeholder URL in `swiftimage-datadisk.yaml`

## Apply

```bash
kubectl apply -f config/samples/shared/swiftguestclass-default.yaml
kubectl apply -f config/samples/shared/swiftseedprofile-minimal.yaml
kubectl apply -f config/samples/disk-boot/swiftimage-ubuntu-noble.yaml
kubectl apply -f config/samples/datadisk/swiftimage-datadisk.yaml
# Wait for both images to be Ready
kubectl apply -f config/samples/datadisk/swiftguest-datadisk.yaml
kubectl get swiftguest datadisk-test -w
```

## Expected result

- SwiftGuest `datadisk-test`: phase=Running
- Inside guest: `lsblk` shows `/dev/vdb` as the data disk

## Cleanup

```bash
kubectl delete swiftguest datadisk-test
kubectl delete swiftimage data-disk ubuntu-noble
```
