# KubeSwift Windows image-prep tooling

Off-cluster tooling to produce a **virtio-ready, Cloud-Hypervisor-bootable**
Windows image for use as a `SwiftImage` (`osType: windows`). A stock Windows ISO
will **not** boot on virtio — it must be prepared first.

Full step-by-step guide (requirements, troubleshooting, all the spike gotchas):
**[`docs/windows/image-prep.md`](../../docs/windows/image-prep.md)**.

| File | Purpose |
|---|---|
| `autounattend.xml` | Unattended Windows Setup answer file: UEFI/GPT, **viostor/NetKVM injection**, keyless eval install, **headless BCD prep** (EMS/SAC + Automatic Repair disabled). Spike-validated. |
| `run-install.sh` | Headless QEMU/KVM install driver — builds the seed ISO, defeats the CD-boot prompt via QMP `send-key`, detects completion, emits a virtio-ready qcow2. |
| `cloudbase-init.conf` | cloudbase-init config pointing at the **NoCloud `cidata`** datasource — the bridge to KubeSwift's provisioning seed. |

## Quickstart

```sh
# Host needs: distro qemu-system-x86_64 (VNC+slirp, NOT kata-static), OVMF (4M),
# genisoimage, python3, KVM. Review/edit autounattend.xml first (password, index).
WIN_ISO=/path/Windows2022.iso VIRTIO_ISO=/path/virtio-win.iso ./run-install.sh
# -> windows.qcow2 (virtio-ready). Then: install cloudbase-init (runbook Part B),
#    host the image, and point a SwiftImage at it (config/samples/windows/).
```

> **Asset-gated:** the produced image is not cluster-validated here (no Windows
> license on the dev cluster). The boot path is spike-proven
> ([`docs/design/windows-guest-support-spike.md`](../../docs/design/windows-guest-support-spike.md)):
> Windows boots cleanly on Cloud Hypervisor v52.0 with `--cpus kvm_hyperv=on`
> (which the KubeSwift runtime adds automatically for `osType: windows`).
