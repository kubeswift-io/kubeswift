# Serial Console Troubleshooting

If `swiftctl console <guest>` shows a blank screen or `socat` from the debug shell shows nothing, the VM is not outputting to the serial port (ttyS0). This is almost always a **kernel cmdline** issue: the kernel needs `console=ttyS0,115200n8` in its boot parameters.

## Root Cause

With firmware boot (CLOUDHV.fd), the kernel cmdline comes from **GRUB on the disk**, not from Cloud Hypervisor. The import job patches GRUB during image import. If the patch didn't run (wrong partition layout, fdisk didn't find partitions), the image has no serial console.

## Fix: Re-import the Image

After updating the import script (e.g. to support Rocky's /boot at 100MiB), you must **re-import** so the GRUB patch runs on a fresh image:

```bash
# 1. Delete the SwiftGuest (stops the VM)
kubectl delete swiftguest rocky -n default

# 2. Delete the SwiftImage (triggers PVC cleanup)
kubectl delete swiftimage rocky9-cloud -n default

# 3. Delete the import PVC (required for fresh import)
kubectl delete pvc swiftimage-import-rocky9-cloud -n default

# 4. Re-apply the SwiftImage
kubectl apply -f config/samples/swiftimage-rocky9.yaml -n default

# 5. Wait for SwiftImage Ready (5-15 min)
kubectl wait --for=jsonpath='{.status.phase}'=Ready swiftimage/rocky9-cloud -n default --timeout=20m

# 6. Check import job logs for "Patched ... for serial console"
kubectl logs job/swiftimage-import-rocky9-cloud -n default | grep -i patch

# 7. Re-apply the SwiftGuest
kubectl apply -f config/samples/swiftguest-rocky.yaml -n default

# 8. Wait for guest Running, then try console
swiftctl console rocky
```

## Verify GRUB Patch Ran

After import, check the job logs:

```bash
kubectl logs job/swiftimage-import-rocky9-cloud -n default
```

You should see `Patched /mnt/disk/grub2/grub.cfg for serial console` (Rocky) or `Patched .../grub.cfg for serial console` (Ubuntu). If you see no "Patched" line, the partition detection failed.

## Manual Test from Debug Shell

From `swiftctl debug rocky --shell`:

```bash
socat -,raw,echo=0 UNIX-CONNECT:/var/lib/kubeswift/run/default-rocky/serial.sock
```

- **If you see boot output or login prompt:** The serial path works. The issue may be swiftctl console (TTY, etc.).
- **If you see nothing:** The VM is not outputting to ttyS0. Re-import with the fixed script.

## Partition Layouts (for import script)

| Distro | Partition | Offset | Path to grub.cfg |
|--------|-----------|--------|------------------|
| Ubuntu | root (227328 sectors) | 116391936 | /boot/grub/, /EFI/ubuntu/ |
| Rocky GenericCloud | /boot (100MiB) | 104857600 | /grub2/grub.cfg |
| Rocky GenericCloud | root (1106MiB) | 1159725056 | /boot/grub2/ |
| Debian | root (134MB) | 140509184 | /boot/grub/ |
