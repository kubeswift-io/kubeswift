# Hot-plugged volumes

A `SwiftVolumeAttachment` attaches a PersistentVolumeClaim to a running
SwiftGuest as a virtio-blk disk, and detaches it, without restarting the
guest. The guest sees the disk at `/dev/disk/by-id/virtio-<serial>`.

The feature is off by default. Turn it on in the chart:

```yaml
swiftGuest:
  volumeHotplug:
    enabled: true
    kubeletRootDir: /var/lib/kubelet   # kubelet's --root-dir on guest nodes
```

Guests started before it was on cannot hot-plug until they restart: their
launcher pod lacks the mount it needs. Their attachments wait with
`LauncherUnsupported`.

## Requirements

- The guest runs on Cloud Hypervisor, has no GPU, and sets
  `spec.guestAgent.enabled: true`. The guest agent tells whether the guest
  still uses a disk before it is unplugged.
- The claim is `volumeMode: Block` and is provisioned by a CSI driver.
- At most 16 attachments per guest.
- A claim can be attached to two guests at once only if it is
  `ReadWriteMany` and every attachment of it is `readOnly`.

## Attach

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftVolumeAttachment
metadata:
  name: db-data
spec:
  guestRef: {name: db-0}
  pvcRef: {name: db-data}
  readOnly: false
```

```bash
kubectl get sva db-data
```

```
NAME      GUEST   PVC       PHASE      SERIAL                 AGE
db-data   db-0    db-data   Attached   ks9f2c41d07b3e8a6c55   12s
```

In the guest:

```bash
ls -l /dev/disk/by-id/virtio-ks9f2c41d07b3e8a6c55
```

The spec is immutable. To change an attachment, delete it and create a new
one.

## Detach

Delete the attachment. It stays `Detaching` until the guest has let go of the
disk: unmount it first. While the guest still mounts it, swaps on it, has a
device-mapper or md device on it, or a process holds it open, the attachment
reports `InUse=True` with what holds it, and the disk stays attached.

```bash
kubectl delete sva db-data
kubectl get sva db-data -o jsonpath='{.status.conditions[?(@.type=="InUse")].message}'
```

For a guest whose agent no longer answers, force it. The guest loses the
disk under whatever was using it:

```bash
kubectl annotate sva db-data kubeswift.io/force-detach=true
```

## Lifecycle

| Event | What happens |
|---|---|
| Guest stopped, or its launcher restarts | The attachment goes `Pending` (`GuestNotRunning`) and the volume leaves the node. When the guest runs again the disk is plugged in again, a few seconds after boot. |
| Guest deleted | The attachment waits (`GuestNotFound`). It is not owned by the guest. |
| In-guest reboot | The disk stays. |
| Offline migration | The disk is plugged in again on the target node. |
| Live migration | Refused while the guest has an attachment. Use `mode: offline`, or detach first. |
| Snapshot with memory (local, s3, oci) | Refused while the guest has an attachment. A `csi-volume-snapshot` (disk only) works and does not include the hot-plugged disk. |

The guest's `status.hotplugVolumes` lists the disks its launcher has.

## How it works

The controller creates an attachment pod on the guest's node that uses the
claim, so kubelet attaches the volume through its CSI driver. The pod is
unprivileged and only sleeps. The controller then writes the guest's set of
volumes onto its launcher pod. The launcher reads the device kubelet
published for the attachment pod, under
`<kubelet root>/plugins/kubernetes.io/csi/volumeDevices/`, which it mounts
read-only, and plugs it into Cloud Hypervisor. It never takes a device number
from an annotation.

## Troubleshooting

`kubectl describe sva <name>` shows the phase, the `Ready` reason and the
events (`VolumeAttached`, `DetachBlocked`, `ForcedDetach`, `VolumeDetached`).

| Reason | Meaning |
|---|---|
| `LauncherUnsupported` | The guest's launcher started before the feature was on. Restart the guest. |
| `AttachingToNode` | The attachment pod is not running yet: the CSI driver is attaching the volume. `kubectl describe pod <sva>-att-…` |
| `PVCNotBound` | The claim waits for its first consumer, or cannot be provisioned. |
| `VolumeUnsupported` | A requirement above is not met; the message says which. |
| `LauncherFailed` | The launcher could not plug the disk in: `NoPCISlot`, `DeviceRefused` (the published device failed a sanity check) or `AddDiskFailed` (Cloud Hypervisor's error). |
| `DetachBlocked` | The guest still uses the disk. |
