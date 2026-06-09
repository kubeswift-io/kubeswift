# Migration — Overview

> Audience: KubeSwift operators
>
> Phase 1 of live migration ships **offline migration**: the source
> guest is fully stopped, its disk is detached on the source node,
> and the guest is recreated on the target node with the same disk
> content. Downtime is measured in seconds (storage detach + VM
> boot), not milliseconds.
>
> Live migration (sub-second downtime via memory pre-copy) is
> reserved for Phase 3.

## What you can do today

- Move any non-VFIO SwiftGuest from one Kubernetes node to another
  with the disk content preserved.
- Use `swiftctl migrate <guest> --to <node>` or apply a
  SwiftMigration manifest directly.
- Cancel an in-flight migration with `swiftctl migration cancel`;
  the controller cleans up source-guest state before the resource
  is GC'd.

## What's NOT in Phase 1

- **Live migration** (`mode: live`) — webhook rejects with a
  Phase-1-not-shipped error. Phase 3 work.
- **Cross-node migration of GPU/SR-IOV guests** — webhook rejects.
  Phase 1 has no release-and-reallocate primitive for the SwiftGPU
  controller; cross-node GPU migration is Phase 4+ work. Same-node
  GPU migrations are pointless (the GPU is already on that node).
- **Drain integration** (`kubectl drain` auto-migrating guests) —
  Phase 4. Operators must currently issue migrations explicitly.
- **mTLS** for the migration channel — Phase 1 has no migration
  channel (offline mode reuses the disk in place); mTLS lands with
  live migration in Phase 3.

## Modes

`spec.mode` accepts `auto`, `live`, or `offline`.

| Mode | Phase 1 behaviour |
|---|---|
| `auto` (default) | Resolved to `offline` (status.mode records the resolution). |
| `live` | **Rejected by the validation webhook** — Phase 3 work. |
| `offline` | Accepted; controller drives the offline state machine. |

## Storage compatibility

Phase 1 uses **direct PVC reuse**: the source guest's per-guest
root-disk PVC is detached on the source node and reattached on the
target node. The CSI driver must support cross-node attach.

| Storage class | Cross-node migration |
|---|---|
| Longhorn (RWO) | Works. Detach window dominates downtime (~45s). |
| Rook Ceph RBD (RWO) | Works. Detach is near-instant (CoW). |
| AWS EBS (RWO) | Works. Detach + reattach in cloud-API time (5-15s). |
| GCE PD (RWO) | Works. Similar to EBS. |
| local-path-provisioner | **Rejected** — volumes are physically tied to one node. |
| RWX storage (CephFS, NFS-CSI) | Works; detach is a no-op since the PV is mountable from any node. |

## Networking compatibility

KubeSwift's default networking is a node-local bridge with per-node
dnsmasq. The guest's IP is local to the node — cross-node migration
produces a **fresh IP** from the destination node's bridge.

The validation webhook rejects cross-node migrations of guests on
default networking unless the operator opts in via
`spec.allowIPChange=true`. When the opt-in triggers, the controller
writes `IPWillChange=True` on `status.conditions` so the change is
visible in `kubectl describe swiftmigration`.

For IP preservation across nodes, attach the guest to a multi-node
network — Multus + macvlan on a shared L2 segment, OVN-K layer-2,
or any other attachment that spans nodes. See
[networking-requirements.md](networking-requirements.md).

## Quick start

```bash
swiftctl migrate db --to worker-3
```

For a guest on default networking:

```bash
swiftctl migrate web --to worker-3 --allow-ip-change
```

### Preflight check

`--check` runs a read-only preflight and creates nothing — handy before a
real move:

```bash
swiftctl migrate db --to worker-3 --check
```

It reports the target node's readiness and capacity, whether the guest's
primary IP is preserved (multi-node NAD) or will change, the mode the
controller would pick (and whether VFIO/SR-IOV forces offline), any
node-local virtiofs/vhost-user backends the target must also provide, and
source-vs-target CPU/architecture compatibility (CPU-feature flags when
node-feature-discovery labels are present, otherwise an advisory to compare
`lscpu` — CPU-feature mismatch is the realistic live-migration failure mode).
Hard blockers (missing guest/target, `--preferred-mode live` for a VFIO
guest) exit non-zero; everything else is a warning.

For full operational details and timing characteristics, see
[offline-migration.md](offline-migration.md).
