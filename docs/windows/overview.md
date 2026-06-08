# Running Windows guests

KubeSwift runs **Windows** VMs as first-class SwiftGuests on **Cloud Hypervisor**
(the same default runtime as Linux), gated by a single field: `osType: windows`.
This page is the operator entry point; the deep dives are linked inline.

> **Status: v1 code-complete, validation asset-gated.** Every layer is implemented
> and individually validated (unit tests + a local boot spike); the only untested
> piece is *in-cluster* cloudbase-init provisioning, which needs a Windows
> image/license the dev cluster doesn't have — the same "asset not available"
> caveat as Tier 2/3 GPU. The boot path itself is proven end-to-end: a virtio-ready
> Windows Server 2022 image boots cleanly on **Cloud Hypervisor v52.0**. See the
> [boot spike](../design/windows-guest-support-spike.md) and the
> [design doc](../design/windows-guest-support.md).

## How it works — `osType: windows`

`osType` (on **SwiftGuest** and **SwiftImage**, default `linux`) is the single
decision point — it mirrors how `gpuProfileRef.tier` selects CH-vs-QEMU. With
`osType: windows`:

| Layer | What changes |
|---|---|
| **Hypervisor** | Cloud Hypervisor disk-boot path (CLOUDHV.fd + virtio), **plus `--cpus kvm_hyperv=on`** — the one runtime setting Windows needs (without it the kernel hangs early). Added automatically. |
| **Image import** | Skips the Linux-only GRUB/serial patch; keeps `qcow2→raw` + size. The import Job runs **unprivileged**. |
| **Provisioning** | **cloudbase-init** reads the *same* NoCloud `seed.iso` (label `cidata`) cloud-init uses on Linux — the seed pipeline is OS-agnostic, no new mechanism. |
| **Console** | Headless (serial/EMS). Manage over **RDP/WinRM**; `swiftctl console` shows SAC, best-effort. |

Webhook rules: `osType: windows` requires disk boot (`imageRef`) — `kernelRef` and
`gpuProfileRef` are rejected in v1 — and the guest's `osType` must match the
referenced image's.

## End-to-end lifecycle

```
1. Prepare a virtio-ready image  ──►  2. Host it (HTTP)  ──►  3. SwiftImage (osType: windows)
        (off-cluster, runbook)                                         │
                                                                       ▼
6. Manage over RDP  ◄──  5. SwiftGuest boots on CH+kvm_hyperv  ◄──  4. SwiftSeedProfile (cloudbase-init)
```

1. **Prepare a virtio-ready image** — a stock Windows ISO will **not** boot on
   virtio. Build one with the [image-prep runbook](image-prep.md) + the
   [`tools/windows-image-prep/`](../../tools/windows-image-prep/) tooling: it
   injects viostor/NetKVM, applies the headless BCD prep, and (Part B) installs
   cloudbase-init for the NoCloud datasource.
2. **Host** the resulting `qcow2` on an HTTP server the cluster can reach.
3. **`SwiftImage`** with `osType: windows`, `source.http.url` → your image.
4. **`SwiftSeedProfile`** (`datasource: NoCloud`) with cloudbase-init user-data
   (hostname, RDP, scripts).
5. **`SwiftGuest`** with `osType: windows`, `imageRef`, `seedProfileRef`.
6. **Manage over RDP** — discover the guest IP in `status.network.primaryIP`.

Ready-to-edit manifests: [`config/samples/windows/`](../../config/samples/windows/).

```sh
kubectl apply -f config/samples/windows/   # after editing the image URL + password
kubectl get swiftguest win-guest -o wide    # watch phase + primaryIP
```

## Requirements

- **Cloud Hypervisor v52.0+** (KubeSwift ships it). v51.1 bugchecks Windows'
  `viostor` driver (`0xD1`); v52.0 fixes it.
- An **operator-prepared, virtio-ready** image (viostor/NetKVM + headless BCD prep
  + cloudbase-init). The runbook automates this.
- Sizing: Windows Server needs ≥ 2 GiB RAM (the default SwiftGuestClass is 4 GiB).

## Limitations (v1 non-goals)

- **GPU passthrough to Windows** — rejected (`osType: windows` + `gpuProfileRef`).
- **Live migration** of Windows guests — not a v1 target.
- **Snapshots** of Windows guests — should work mechanically, but not a v1
  validation target.
- **Kernel boot** — Windows is disk-boot only.

## See also

- [Windows image prep runbook](image-prep.md)
- [Windows samples](../../config/samples/windows/)
- [Design doc](../design/windows-guest-support.md) · [boot spike](../design/windows-guest-support-spike.md)
