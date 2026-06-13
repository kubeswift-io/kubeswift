# Windows guest samples

Running a Windows guest as a SwiftGuest. **Windows runs on Cloud Hypervisor**
(v52.0+) via the same disk-boot path Linux uses — the `osType: windows` gate is
the single decision point.

> The full in-cluster Windows path is **cluster-validated** (Cloud Hypervisor
> v52.0; boot → NetKVM/DHCP → cloudbase-init over NoCloud → Running+IP); see
> [`docs/windows/overview.md`](../../../docs/windows/overview.md).
> These manifests are still **illustrative**: the image URL is a placeholder —
> point `source.http.url` at your own operator-prepped, virtio-ready image
> (build one with [`tools/windows-image-prep/`](../../../tools/windows-image-prep/)).

## The three pieces

| File | What it does |
|---|---|
| `swiftimage-windows.yaml` | `SwiftImage` with `osType: windows` — import skips the Linux-only GRUB/serial patch (PR 3), keeps `qcow2→raw`. Source **must** be an operator-prepared, virtio-ready image. |
| `swiftseedprofile-windows.yaml` | `SwiftSeedProfile` (NoCloud) consumed by **cloudbase-init** — the *same* `cidata` seed mechanism cloud-init uses (no new datasource). |
| `swiftguest-windows.yaml` | `SwiftGuest` with `osType: windows` → CH disk-boot + `--cpus kvm_hyperv=on` (PR 4). |

```sh
kubectl apply -f swiftimage-windows.yaml -f swiftseedprofile-windows.yaml -f swiftguest-windows.yaml
```

## Prerequisites (operator-prepared image)

A **stock Windows ISO will not boot on virtio.** The image referenced by the
`SwiftImage` must be prepared off-cluster (PR 6 runbook; the spike's
`autounattend.xml` + `run-install.sh` are the seed for that tooling):

1. **virtio drivers** pre-installed — `viostor` (boot disk) + `NetKVM` (NIC).
2. **Headless BCD prep** — EMS/SAC on serial, Automatic Repair disabled
   (`recoveryenabled no` + `bootstatuspolicy ignoreallfailures`), so a fallback-
   path boot goes straight to the OS instead of a graphical recovery screen that
   hangs on a console-less VMM.
3. **cloudbase-init** installed and configured for the **NoCloud `cidata`**
   metadata service, so it discovers the seed disk this `SwiftSeedProfile`
   produces.

## Provisioning model

KubeSwift's seed pipeline is **OS-agnostic**: it builds a NoCloud `seed.iso`
(volume label `cidata`) with flat `user-data` / `meta-data` / `network-config`
files. cloud-init reads it on Linux; **cloudbase-init reads the identical seed on
Windows** — so there is no Windows-specific seed code. The sample sets the
hostname from `meta-data: local-hostname` and runs a PowerShell `user-data`
(`#ps1_sysnative`) to enable RDP on first boot. cloudbase-init accepts a subset
of cloud-config plus plain scripts (PowerShell / cmd).

## Console & management

Windows on CH is **headless** (serial/EMS). Manage over **RDP** (enabled by the
seed above) or WinRM; `swiftctl console` (serial) shows SAC and is best-effort.
GPU passthrough, live migration, and snapshots of Windows guests are **not** v1
targets.
