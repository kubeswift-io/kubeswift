# Preparing a Windows image for KubeSwift

> Operator runbook. Produces a **virtio-ready, Cloud-Hypervisor-bootable** Windows
> image to import as a `SwiftImage` (`osType: windows`). This is an **off-cluster**
> procedure — run it on a Linux host with QEMU/KVM, then host the resulting image
> and point a `SwiftImage` at it.
>
> Why this is needed: a **stock Windows ISO cannot boot on virtio** (no in-box
> virtio-blk/net drivers), and a console-less VMM (Cloud Hypervisor is
> serial/headless) needs the boot configured so Windows doesn't drop into a
> graphical recovery screen. The tooling here bakes in both. The boot path is
> validated end-to-end on Cloud Hypervisor v52.0.

Tooling: [`tools/windows-image-prep/`](../../tools/windows-image-prep/).

## 0. Requirements (the prep host)

- **`qemu-system-x86_64` — a distro build** (Debian/Ubuntu `/usr/bin/...`) with
  **VNC + slirp + std-VGA**. A stripped `kata-static` QEMU lacks all three and
  will not work (it rejects `-vnc`/`-netdev user` and dies on the default VGA).
- **OVMF** (4 MB build: `OVMF_CODE_4M.fd` + `OVMF_VARS_4M.fd`), **`genisoimage`**,
  **`python3`**, and **KVM** (`/dev/kvm`).
- **Windows installation ISO** — e.g. the Windows Server 2022 **evaluation** ISO
  (180-day, keyless), or your licensed media.
- **`virtio-win.iso`** — the stable
  [virtio-win](https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/stable-virtio/virtio-win.iso)
  driver ISO.

## 1. Part A — automated virtio install (validated)

Review `tools/windows-image-prep/autounattend.xml` first and change the
**`/IMAGE/INDEX`** if you want a different edition. Index `1` is *Standard
(Server Core)*. Check your media: `wiminfo /mnt/iso/sources/install.wim`
(`apt install wimtools`); use `2` for *Standard (Desktop Experience)*.

The Administrator password and the virtio driver version are passed in via env
(no need to hand-edit the answer file):

```sh
cd tools/windows-image-prep
# Windows Server 2022:
WIN_ISO=/path/Windows2022.iso VIRTIO_ISO=/path/virtio-win.iso \
  ADMIN_PASSWORD='Str0ng-Passw0rd!' ./run-install.sh
# Windows Server 2025 (WIN_VER selects the virtio-win driver folder; the ISO
# ships per-OS folders \viostor\2k25\..., \NetKVM\2k25\..., etc.):
WIN_ISO=/path/Windows2025.iso VIRTIO_ISO=/path/virtio-win.iso \
  WIN_VER=2k25 ADMIN_PASSWORD='Str0ng-Passw0rd!' \
  CLOUDBASE_MSI=/path/CloudbaseInitSetup_Stable_x64.msi \
  OUT_QCOW2=./windows2025.qcow2 ./run-install.sh
```

Key env (see the script header for all): `WIN_VER` (default `2k22`; `2k25` for
Server 2025, `2k19` for 2019, …) selects the virtio-win driver folder;
`ADMIN_PASSWORD` bakes a real password in; **`CLOUDBASE_MSI`** stages
cloudbase-init onto the seed so it installs **offline during the same run** —
folding Part B into Part A (skip Part B entirely if you pass it).

What it does (fully headless, ~10–40 min): builds the answer-file seed ISO, boots
QEMU/KVM, **defeats the "Press any key to boot from CD" prompt** via QMP
`send-key`, injects **viostor/NetKVM** in WinPE so Setup sees the virtio-blk disk,
installs onto a UEFI/GPT layout, runs the **headless BCD prep**, optionally
installs cloudbase-init from the seed, writes a `KUBESWIFT_PREP_OK` sentinel to
the serial port, and powers off. Output: `windows.qcow2` (or `OUT_QCOW2`). It
auto-picks a free VNC display + RDP-forward port and prints them — connect a VNC
client there to watch.

> **Validated on Windows Server 2025** (build 26100, Standard Core, `virtio-win`
> stable `2k25` drivers): the produced image boots from the virtio disk and
> brings up the **SAC serial console** headlessly — viostor injection + the
> EMS/SAC BCD prep both confirmed.

The result is a **virtio-ready, headless-bootable** image. The headless BCD prep
it applies (the spike's key fix) is:

```
bcdedit /emssettings EMSPORT:1 EMSBAUDRATE:115200
bcdedit /ems {current} on
bcdedit /bootems {bootmgr} on
bcdedit /set {current} bootstatuspolicy ignoreallfailures
bcdedit /set {current} recoveryenabled no
```

## 2. Part B — install cloudbase-init for NoCloud provisioning

So the guest applies the KubeSwift seed (hostname / users / scripts) on first boot,
install **cloudbase-init** and point it at the **NoCloud `cidata`** datasource —
the same seed cloud-init reads on Linux (KubeSwift's seed pipeline is OS-agnostic).

Boot the image once with networking (or do this inside the install via a
`FirstLogonCommand`), then in an elevated PowerShell:

```powershell
# Download + silently install cloudbase-init.
$url = "https://www.cloudbase.it/downloads/CloudbaseInitSetup_Stable_x64.msi"
Invoke-WebRequest $url -OutFile C:\cbi.msi
Start-Process msiexec -ArgumentList '/i C:\cbi.msi /qn /norestart' -Wait

# Drop in the KubeSwift NoCloud config (from tools/windows-image-prep/).
Copy-Item .\cloudbase-init.conf `
  "C:\Program Files\Cloudbase Solutions\Cloudbase-Init\conf\cloudbase-init.conf" -Force
```

The provided [`cloudbase-init.conf`](../../tools/windows-image-prep/cloudbase-init.conf)
sets `metadata_services` to the NoCloud service and `config_drive_locations =
cdrom` (KubeSwift attaches the seed as a CD). cloudbase-init then reads the seed's
`meta-data` (`local-hostname`, …) and runs the `user-data` — see
[`config/samples/windows/swiftseedprofile-windows.yaml`](../../config/samples/windows/swiftseedprofile-windows.yaml)
for a working example (a `#ps1_sysnative` PowerShell user-data enabling RDP).

## 3. Part C — generalize into a template (optional, recommended)

For a reusable template where **each** guest gets a fresh identity and re-runs
cloudbase-init, sysprep-generalize before capturing:

```powershell
$cbi = "C:\Program Files\Cloudbase Solutions\Cloudbase-Init"
C:\Windows\System32\Sysprep\sysprep.exe /generalize /oobe /shutdown `
  /unattend:"$cbi\conf\Unattend.xml"
```

cloudbase-init ships `conf\Unattend.xml` (and a `cloudbase-init-unattend.conf`)
that re-arms it for the specialize pass. Skip Part C only if you intend to run a
single guest from the image.

## 4. Import as a SwiftImage

Host `windows.qcow2` on an HTTP server reachable by the cluster, then:

```sh
kubectl apply -f config/samples/windows/swiftimage-windows.yaml      # set spec.source.http.url
kubectl apply -f config/samples/windows/swiftseedprofile-windows.yaml
kubectl apply -f config/samples/windows/swiftguest-windows.yaml
```

The import skips the Linux-only GRUB/serial patch for `osType: windows` and runs
unprivileged (it keeps only `qcow2→raw` + size measurement). The runtime routes
the guest to Cloud Hypervisor with `--cpus kvm_hyperv=on`. Manage the booted guest
over **RDP** (the sample seed enables it); Windows on CH is headless.

## 5. Troubleshooting (lessons from the spike)

| Symptom | Cause / fix |
|---|---|
| `-vnc`/`-netdev user`/`vgabios-stdvga.bin` errors | You're on a **kata-static** QEMU. Use the distro `/usr/bin/qemu-system-x86_64`. |
| Install hangs at a black/early screen, no disk writes | The CD-boot "Press any key" prompt wasn't satisfied — the driver's QMP `send-key` spam handles it; for manual installs, press a key over VNC. |
| Setup stalls on the **language** screen | The answer file needs the `Microsoft-Windows-International-Core-WinPE` component (included). |
| "Windows cannot find the Microsoft Software License Terms" | A `<ProductKey>` element on **eval** media — remove it (the answer file does). Edition comes from `/IMAGE/INDEX`. |
| Setup error "we couldn't find any drives" | viostor not injected — check the `DriverPaths` letters/paths match your `virtio-win.iso` layout (`\viostor\2k22\amd64`). |
| Boots to a graphical **"Automatic Repair"** that hangs on CH | Missing headless BCD prep — `recoveryenabled no` + `bootstatuspolicy ignoreallfailures` (the answer file applies these). |
| Windows silently hangs early on CH (no SAC) | Missing `kvm_hyperv` — the KubeSwift runtime adds `--cpus kvm_hyperv=on` for `osType: windows` automatically. |
| `0xD1 DRIVER_IRQL_NOT_LESS_OR_EQUAL` in `viostor.sys`, reboot loop | A **CH v51.1** virtio-blk bug. Fixed in **CH v52.0** (KubeSwift ships v52.0). |

## 6. Scope

v1 targets disk-boot Windows guests managed over RDP/WinRM, provisioned by
cloudbase-init over the NoCloud seed. **Not** v1: GPU passthrough to Windows,
Windows live migration, and Windows snapshots as a validation target.
