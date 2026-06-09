# vhost-user devices (virtiofs first)

> Status: DESIGN. Grounded against the CH v52.0 binary + the current swiftletd
> runtime. Adopts CH v52's generic vhost-user (#7221) + first-class virtio-fs.
> v1 ships **virtiofs** (host-dir / PVC sharing into the guest); vhost-user
> net/blk and the generic device are phased follow-ons. Last updated: 2026-06-09.

## 1. Goal

Let KubeSwift guests use **vhost-user devices** — virtio devices whose backend
runs as a separate userspace process talking to the VMM over a unix socket +
shared guest memory. This unlocks:

- **virtiofs** (`--fs`) — share a host directory or PVC into the guest as a
  POSIX filesystem (`mount -t virtiofs <tag> /mnt`). The concrete, broadly
  useful, self-contained first device (config/data/model sharing, golden
  content, scratch space). **v1 scope.**
- **vhost-user-net** — high-throughput networking via an **operator-provided**
  DPDK / OVS-DPDK backend (telco/NFV). **v1 arc.** Unlike virtiofs there is no
  bundled backend: vhost-user-net only beats the existing tap/bridge path when
  paired with a userspace fast-path (DPDK), which is node-level operator infra.
  KubeSwift plumbs CH to the operator's backend socket; it does not run the
  backend. (See §3.5.)
- **vhost-user-blk** — high-throughput block via an SPDK backend. Later phase.
- **generic vhost-user device** (`--user-device`) — arbitrary backends by
  `virtio_id`. Later phase.

**v1 ships virtiofs AND vhost-user-net.** virtiofs is the turnkey,
fully-cluster-validatable device (bundled virtiofsd); vhost-user-net is the
operator-socket device whose *wiring* is validatable here but whose full DPDK
datapath is asset-gated (the dev cluster has no DPDK/SR-IOV NICs — same posture
as the existing SR-IOV code: shipped, hardware validation deferred).

**Prerequisite — already satisfied.** vhost-user backends access guest buffers
through **shared guest memory**. KubeSwift already maps guest RAM as a shared
memfd via `--memory ...,shared=on` (PR #165, shipped to fix the snapshot OOM).
So the shared-memory plumbing vhost-user needs is **already in place** — no
memory-backing change is required to add vhost-user devices.

## 2. CH v52 surface (confirmed against the binary)

- `--fs tag=<tag>,socket=<sock>,num_queues=<n>,queue_size=<q>,id=<id>` — virtio-fs.
- `--user-device virtio_id=<id|name>,socket=<sock>,queue_sizes=<list>,id=<id>` — generic.
- `--net ...,vhost_user=on,socket=<sock>` and `--disk ...,vhost_user=on,socket=<sock>` — vhost-user net/blk.

`swift-ch-client/config.rs::to_args` emits `--disk`/`--net`/`--device` today; it
gains `--fs` (v1) and later the vhost-user variants.

## 3. v1 architecture — virtiofs

```
Launcher pod (RestartPolicy: Never, minimal caps, --memory shared=on)
 ├─ source volume(s): hostPath OR PVC, mounted read[-only] at
 │     /var/lib/kubeswift/virtiofs/<name>
 └─ launcher container (swiftletd)
        ├─ swiftletd spawns, per filesystem, BEFORE Cloud Hypervisor:
        │     virtiofsd --socket-path=<run>/<name>.fs.sock \
        │               --shared-dir=/var/lib/kubeswift/virtiofs/<name> \
        │               --sandbox none [--readonly]
        ├─ waits for each .fs.sock to appear
        └─ spawns Cloud Hypervisor with, per filesystem:
              --fs tag=<tag>,socket=<run>/<name>.fs.sock,num_queues=1,queue_size=1024
Guest: mount -t virtiofs <tag> /mnt   (operator / cloud-init / seed)
```

**Why virtiofsd as a swiftletd-spawned child (not a sidecar):** it shares the
runtime dir (for the socket) and the source mount with CH naturally, needs no
cross-container ordering, and its lifecycle ties to swiftletd (kill on CH exit)
— mirroring how swiftletd already owns the CH process. A sidecar would add
ordering + a second image for no benefit at v1. (The migration-stunnel sidecar
exists because it must bridge the *cross-pod* TLS hop; virtiofsd is *in-pod*.)

**Image:** add `virtiofsd` (the rust-vmm daemon, apt-packaged on
`debian:bookworm-slim`) to the swiftletd runtime stage.

## 3.5 v1 architecture — vhost-user-net (operator-provided backend)

```
Node: operator runs a DPDK fast-path (OVS-DPDK / a DPDK vswitch) that exposes a
      vhost-user-net listener socket per interface, e.g. /var/run/vhost/<id>.sock
Launcher pod
 ├─ hostPath volume: the backend socket dir, mounted into the launcher
 └─ swiftletd spawns CH with, per vhost-user NIC:
       --net vhost_user=on,socket=/var/run/vhost/<id>.sock,mac=<mac>,num_queues=<n>
```

Unlike virtiofs, **KubeSwift does not run the net backend** — the operator owns
the DPDK datapath (a node DaemonSet / host service), exactly as SR-IOV expects
the VFs to be pre-provisioned. KubeSwift's job is to (1) surface a NIC `type:
vhost-user` with the backend `socket`, (2) mount that socket into the launcher,
and (3) plumb `--net ...,vhost_user=on,socket=...` to CH. The guest sees an
ordinary virtio-net device; the line-rate comes from the backend.

CRD: extend the existing `GuestInterface` with `type: bridge | sriov |
vhost-user` (today bridge/sriov). A `vhost-user` interface carries `socket`
(node path) instead of a `networkRef`/`resourceName`:

```yaml
spec:
  interfaces:
  - name: fast0
    type: vhost-user
    socket: /var/run/vhost/fast0.sock   # operator backend listener (hostPath)
    mac: "..."                          # optional; generated if unset
```

**Validation reality:** the dev cluster has no DPDK NIC, so PR 3 validates the
*wiring* (CH spawns with `vhost_user=on,socket=...`; the arg is correct; webhook
accepts/bounds it) and documents that the line-rate datapath is asset-gated —
the same honest posture as the shipped-but-hardware-unvalidated SR-IOV path.

## 4. Security (Design Principle: no privileged containers)

- **`--sandbox none`.** virtiofsd's default `namespace` sandbox needs
  `CAP_SYS_ADMIN`; the project forbids that. With `--sandbox none`, **the
  launcher container/pod IS the security boundary** — virtiofsd can only touch
  the explicitly-mounted `--shared-dir`, nothing else. This is the standard
  Kubernetes-pod posture and adds **no** capabilities.
- **No `privileged: true`; no new caps.** Confirmed against the binary that
  `--sandbox none` needs none.
- **uid/gid model (v1, documented):** virtiofsd runs as the launcher container's
  uid; files the guest creates are owned by that uid on the host backing store.
  Operators size the source ownership accordingly. id-mapped mounts /
  `--translate-uid` are a follow-up refinement.
- **`readOnly`** sources pass `--readonly` so a guest cannot mutate shared
  golden content.
- The shared-memfd (`shared=on`) is already the guest's backing; virtiofsd maps
  it to DMA into guest buffers — no extra exposure beyond the guest's own RAM.

## 5. CRD surface — `SwiftGuest.spec.filesystems[]`

```yaml
spec:
  filesystems:
  - name: shared              # device id, unique per guest; drives socket/mount paths
    tag: myshare              # guest mount tag: `mount -t virtiofs myshare /mnt`
    source:                   # exactly one of:
      hostPath: /data/share   #   node-local dir (DirectoryOrCreate)
      # pvcRef: {name: data}  #   a PVC (use RWX for shared/multi-guest)
    readOnly: false           # default false
```

- New `Filesystem` type + `Filesystems []Filesystem` on `SwiftGuestSpec`; a
  `SwiftGuestClass.spec.filesystems` default + per-field merge (mirrors storage).
- **Boot-source-agnostic:** works for `imageRef`, `kernelRef`, and clones — it's
  a virtio device, independent of the boot path.
- **Webhook (per-operation discipline):** unique `name` + `tag` per guest;
  `source` is exactly one of hostPath/pvcRef; tag length/charset (virtiofs tag
  ≤ 36 bytes); reject on the GPU/QEMU path in v1 (CH-path only — see §7).

## 6. Plumbing (mirrors existing disk/intent flow)

- **Resolver → RuntimeIntent:** `RuntimeIntent.Filesystems[]` (tag,
  in-pod-source-path, socket-path, readOnly).
- **Pod builder:** add a volume + mount per filesystem (hostPath
  `DirectoryOrCreate`, or the PVC) at `/var/lib/kubeswift/virtiofs/<name>`,
  reusing the `applyDataDiskRefs` pattern in `pod.go`.
- **swiftletd:** a `spawn_virtiofsd` step before CH launch (per fs), socket-wait,
  then `--fs` args; kill virtiofsd children on CH exit (drop-guard, like the
  migration progress emitter).
- **swift-ch-client `config.rs`:** emit `--fs tag=...,socket=...,num_queues=1,queue_size=1024`
  per filesystem. Opaque-path contract preserved (no path-shape inference).

## 7. Boot/runtime compatibility

- **CH path (default):** v1 target. Disk-boot + kernel-boot both supported.
- **QEMU/GPU path:** QEMU also does virtiofs (`-chardev socket` +
  `-device vhost-user-fs-pci` + virtiofsd). A follow-on PR adds it to
  `swift-qemu-client`; v1 webhook rejects `filesystems` on the QEMU path.
- **Live migration:** vhost-user device state does not live-migrate cleanly
  (the backend is external) — v1 treats a guest with `filesystems` like VFIO for
  migration (offline only / live-rejected). Stated explicitly; revisit later.
- **Snapshots:** a virtiofs device references an external backend + host dir, not
  guest-internal state; snapshot/clone of a guest with `filesystems` is allowed
  (the share re-attaches on resume) but the *shared content* is not part of the
  snapshot. Document.

## 8. Phased PRs

| PR | Scope | Gate |
|---|---|---|
| 1 | This design doc. | — |
| 2 | **virtiofs (CH path)**: `virtiofsd` in the swiftletd image; `spec.filesystems` CRD + class default + webhook; resolver/RuntimeIntent; pod volumes/mounts; swiftletd `spawn_virtiofsd` + lifecycle; `swift-ch-client --fs`. On-cluster: hostPath + PVC share, in-guest mount + read/write, readOnly enforcement, kernel-boot + disk-boot. | After PR 1 |
| 3 | **vhost-user-net (operator backend)**: `GuestInterface.type: vhost-user` + `socket`; webhook; resolver/RuntimeIntent; hostPath socket mount; `swift-ch-client --net vhost_user=on,socket=...`. On-cluster: wiring validated (CH spawns with the socket; arg correct); full DPDK datapath asset-gated (no DPDK NIC). The "vhost-user" SwiftKernel profile (tuned for the userspace fast path) can ride here or follow. | After PR 2 |
| 4 | QEMU-path virtiofs + vhost-user-net (`swift-qemu-client` + lift the webhook GPU rejection). | hardware-independent (fs); DPDK-gated (net) |
| 5 | vhost-user-**blk** (SPDK) + generic `--user-device`. | demand |

## 9. Open questions (resolved during PR 2)
1. **virtiofsd packaging** — `apt-get install virtiofsd` on bookworm vs vendoring
   a pinned static binary (version pinning + provenance). Decide in PR 2.
2. **uid/gid mapping** — v1 ships the container-uid model (§4); evaluate
   id-mapped mounts / `--translate-uid` as a follow-up if operators need guest↔host
   uid fidelity.
3. **DAX** (shared cache window for near-native mmap perf) — needs a CH cache
   region; defer (perf tuning, not correctness).
4. **Default `num_queues`/`queue_size`** — start 1/1024; tune from a PR 2
   throughput measurement.

🤖 Generated with [Claude Code](https://claude.com/claude-code)
