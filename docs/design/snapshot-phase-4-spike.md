# Snapshot Phase 4 spike — findings

> Validates the design in
> [`snapshot-phase-4-clonefromsnapshot.md`](snapshot-phase-4-clonefromsnapshot.md)
> before implementation. Run 2026-06-04 on the dev cluster (k0s 1.34, CH v51.1,
> boba + miles, in-cluster MinIO), controller `sha-5c3bc95`. **PASS** — Option A
> (SwiftGuest-native clone boot reusing the restore-receive path) is de-risked.

## Setup

`p4-source` (rocky9, identity-regen seed) on boba, sentinel
`P4-SPIKE-1780562622` written to `/home/kubeswift/sentinel.txt`, then a Tier C
(s3) `SwiftSnapshot p4-snap` → Ready (`s3://kubeswift-snapshots/p4/default/
p4-snap/`). Two `SwiftRestore` clones from the **same** snapshot, pinned to
different nodes: `p4-clone-a` → boba, `p4-clone-b` → miles. (SwiftRestore is the
proven driver today; it exercises the exact restore-receive + identity machinery
Option A will reuse from the SwiftGuest controller.)

## Results

| Scenario | Result |
|---|---|
| **S1** — clone boots from an s3-downloaded cache via the restore-receive path | **PASS** — both clones downloaded independently on their own node and reached `Running` |
| **S2** — two clones, different nodes, coexist with distinct hypervisor MACs | **PASS** — `p4-clone-a` (boba) `52:54:00:23:87:b7`, `p4-clone-b` (miles) `52:54:00:2c:31:3e` — deterministic per `(ns,name,iface)`, distinct |
| **S3** — resume-vs-boot identity limitation reproduces | **PASS** — see below |

### S3 detail — identity across source + both clones

| | sentinel | /etc/machine-id | in-guest eth0 MAC |
|---|---|---|---|
| p4-source (boba) | `P4-SPIKE-1780562622` | `9a3abf29…376a` | `2e:e2:6f:84:8d:a1` |
| p4-clone-a (boba) | `P4-SPIKE-1780562622` | `9a3abf29…376a` | `2e:e2:6f:84:8d:a1` |
| p4-clone-b (miles) | `P4-SPIKE-1780562622` | `9a3abf29…376a` | `2e:e2:6f:84:8d:a1` |

- **Sentinel present on all three** → clone-from-snapshot resumes the captured
  state byte-for-byte (the core Phase 4 capability).
- **machine-id + guest-visible MAC identical** → the documented resume-vs-boot
  limitation reproduces exactly: cloud-init does not re-run on CH `--restore`, so
  clones inherit guest-visible identity until their first reboot (the seed
  `kubeswift.clone=true` bootcmd then regenerates machine-id / hostname / SSH
  host keys — prior-validated in Scenario 6, not re-run here to avoid a
  pod-killing guest reboot).

## Key architectural finding — collision-safety is netns, not guest-MAC

The guest-visible eth0 MAC is **identical** across clones (cached in virtio-net
state at capture), yet the two clones coexist with **no L2 collision**. Why:

- Each clone runs in its **own pod network namespace** (Kubernetes-isolated, its
  own `br0`). Two clones are never on the same L2 segment, even on the same node.
- The **hypervisor** `config.net[].mac` is rewritten **distinct per clone**
  (host-side tap/bridge fdb), which is what `computeMACRewrites` already does.

So **N coexisting clones are safe by construction** — per-pod netns isolation +
the deterministic hypervisor-MAC rewrite. The guest-visible identity collision
(MAC/machine-id/hostname) is a *usability* concern (an operator reboots each
replica to diverge), **not** a collision-avoidance blocker for pools. This is the
single most important finding for the pool design: a Phase 4 pool can stand up N
replicas immediately; reboot-to-diverge is an operator choice, not a correctness
requirement.

## Open questions resolved

- **OQ3 (per-guest PVC):** RESOLVED — no per-guest root PVC needed. Both clones
  booted from `/var/lib/kubeswift/restore/staging/config.json` (the
  snapshot-stager copies the snapshot into a pod-local `emptyDir` and patches
  `config.json`/`disks[].path`/`serial.socket` per clone). The restore-receive
  launcher runs CH `--restore` against that staged dir. A pool replica therefore
  needs **no** `volumeClaimTemplate` for the root disk (it may still want a PVC
  for a `dataDiskRef`, OQ4).

## Still open (carried to design review / later PRs)

- **OQ1** — pool node pre-assignment vs float-then-pin. The spike pinned both
  clones via explicit `targetNode`; the pool must supply this per replica.
  Leaning pre-assign (deterministic; mirrors the migration target-selection).
- **OQ2** — snapshot-lifetime enforcement while a pool references it (finalizer
  vs webhook-reject-delete).
- **OQ4** — `dataDiskRef` / `spec.storage` (RWX+Block) interaction with a
  clone-booted guest.

## Disposition

Option A confirmed. The restore-receive boot, the per-clone hypervisor MAC, and
per-pod netns isolation all hold for the multi-clone case. Proceed to **PR 1**
(extract `internal/snapshot/clonecommon`), then the CRD + SwiftGuest-native
clone-boot path. Spike branch is **non-merge**; this findings doc lands with the
design.
