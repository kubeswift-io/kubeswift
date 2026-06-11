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
# genisoimage, python3, KVM. Check /IMAGE/INDEX in autounattend.xml for your edition.

# Windows Server 2022:
WIN_ISO=/path/Windows2022.iso VIRTIO_ISO=/path/virtio-win.iso \
  ADMIN_PASSWORD='Str0ng-Passw0rd!' ./run-install.sh

# Windows Server 2025 — set WIN_VER=2k25 (the virtio driver folder) and pass
# CLOUDBASE_MSI to install cloudbase-init offline in the same run (no Part B):
WIN_ISO=/path/Windows2025.iso VIRTIO_ISO=/path/virtio-win.iso WIN_VER=2k25 \
  ADMIN_PASSWORD='Str0ng-Passw0rd!' \
  CLOUDBASE_MSI=/path/CloudbaseInitSetup_Stable_x64.msi \
  OUT_QCOW2=./windows2025.qcow2 ./run-install.sh
# -> windows2025.qcow2 (virtio-ready + cloudbase-init). Host it, point a
#    SwiftImage at it (config/samples/windows/).
```

Key env: `WIN_VER` (virtio-win driver folder: `2k22`/`2k25`/`2k19`/…),
`ADMIN_PASSWORD`, `CLOUDBASE_MSI` (stages cloudbase-init for offline install —
Part B folded into Part A), `OUT_QCOW2`, `DISK_GB`, `MEM_MB`/`CPUS`. The VNC
display + RDP-forward port are auto-picked and printed.

> **Windows Server 2025 validated off-cluster:** the WS2025 image (build 26100,
> Standard Core, virtio-win stable `2k25`) boots from the virtio disk to the
> **SAC serial console** headlessly — viostor + EMS/SAC prep confirmed. It is
> not *cluster*-validated (no Windows license on the dev cluster); the CH path is
> spike-proven
> ([`docs/design/windows-guest-support-spike.md`](../../docs/design/windows-guest-support-spike.md)):
> Windows boots on Cloud Hypervisor v52.0 with `--cpus kvm_hyperv=on` (which the
> KubeSwift runtime adds automatically for `osType: windows`).
