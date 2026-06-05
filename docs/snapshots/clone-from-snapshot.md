# Clone from snapshot (Snapshot Phase 4)

`SwiftGuest.spec.cloneFromSnapshot` boots a guest as a **clone of a
SwiftSnapshot** — the guest resumes the captured memory state byte-for-byte
(Cloud Hypervisor `--restore`) with a per-clone hypervisor identity. Templated
on a `SwiftGuestPool`, it spins up **N VMs all cloned from one snapshot** —
seconds per clone instead of minutes-per-cloud-image-boot.

This is the ergonomic layer over the Phase 1/2/3 snapshot primitives; it adds no
new runtime mechanism (it reuses the restore-receive launcher + the s3 download).

## When to use it

- Fast horizontal scale-out from a *warmed* VM (a memory snapshot captures a
  running, post-boot, post-init state).
- A pool of identical workers cloned from one golden snapshot.

For a single disk-only clone or pool scaling from a *disk* image, use
`SwiftImage.cloneStrategy: snapshot` (Tier A) instead — that clones an image's
PVC, not a running VM's full state.

## The boot source

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
spec:
  cloneFromSnapshot:
    snapshotRef:
      name: my-snapshot        # a Ready SwiftSnapshot in the same namespace
    targetNode: boba           # REQUIRED for a Tier C (s3) snapshot; ignored for Tier B
    regenerate:                # identity reset on the clone (default: all four)
      - macAddresses
      - hostname
      - machineId
      - sshHostKeys
```

`cloneFromSnapshot` is **mutually exclusive** with `imageRef`, `kernelRef`, and
`gpuProfileRef` (VFIO state cannot be CH-restored). `guestClassRef` is **still
required** by the CRD schema (set it to any class) but is not used for
resources — the resumed VM's CPU/memory come from the snapshot.

## How it works

```
SwiftGuest (cloneFromSnapshot)
  → controller resolves the snapshot (Ready) + the LIVE source guest
  → Tier C: a node-pinned download Job pulls the artifacts into the target node's cache
  → self-stamps the clone-mode restore annotations (per-clone MAC, runtime-dir rewrites)
  → restore-receive launcher boots: snapshot-stager stages+patches config.json,
    swiftletd runs CH --restore from the node-local cache
  → the clone resumes the captured memory; a fresh root disk is provisioned from
    the source's image
```

**The source guest must be live.** The snapshot records only a small
`CapturedGuestSpec` (CPU/mem/image-name) for validation; the clone needs the full
source spec (image / seed / class) to build its pod. A clone of a snapshot whose
source guest was deleted fails with a clear message. (Cross-cluster / source-gone
clones are a future enhancement.)

**Tier B vs Tier C:**

| Backend | Clone runs on | Notes |
|---|---|---|
| `local` (Tier B) | the **capture node** (`targetNode` ignored) | same-node only; no download |
| `s3` (Tier C) | the node named by `targetNode` | a download Job populates that node's cache first — the cross-node / pool fit |

## Pools — N clones from one snapshot

`SwiftGuestPool.spec.template.spec.cloneFromSnapshot` clones every replica from
the snapshot. The pool **pre-assigns each replica a `targetNode`** (round-robin
across schedulable worker nodes), so Tier C clones spread across the cluster.

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestPool
spec:
  replicas: 2
  template:
    spec:
      runPolicy: Running
      cloneFromSnapshot:
        snapshotRef: {name: my-snapshot}
        regenerate: [macAddresses, hostname, machineId, sshHostKeys]
```

> ℹ️ **`replicas` may exceed the worker-node count.** When more replicas than
> nodes land on the same node from the same snapshot, they **share one Tier C
> download Job** (keyed per `(node, snapshot)`), so they no longer race on the
> shared snapshot-keyed node cache. (The earlier "keep `replicas` ≤ nodes"
> guidance is lifted.) Replicas are still round-robined across nodes for spread.

## Identity (the resume-vs-boot rule)

CH `--restore` resumes byte-for-byte — **cloud-init does not re-run** — so without
intervention every clone inherits the source's identity. Two layers handle it:

- **MAC**: the controller rewrites each clone's *hypervisor* `config.net[].mac` to
  a value deterministic in `(namespace, name, iface)` — **always**, regardless of
  the `regenerate` list. Combined with each clone's own pod network namespace,
  this makes N coexisting clones collision-safe **by construction** (the
  guest-visible MAC stays the source's until reboot, but the clones are never on
  the same L2 segment).
- **hostname / machine-id / SSH host keys**: regenerated on each clone's **first
  reboot** via the seed profile's `kubeswift.clone=true` bootcmd (listed in
  `regenerate`). Until a reboot they are inherited.

**To diverge guest-visible identity, reboot each replica once after it resumes.**
(An in-guest vsock agent to do this without a reboot is a future enhancement.)

## Quick start (walkthrough)

```bash
# Prereqs: in-cluster MinIO + bucket, the identity-regen seed, and s3 creds.
kubectl apply -f config/samples/s3-snapshots/00-minio.yaml      # + create the bucket
kubectl apply -f config/samples/local-snapshots/01-seed-profile.yaml
kubectl apply -f config/samples/s3-snapshots/01-s3-creds.yaml

kubectl apply -f config/samples/clone-from-snapshot/02-source-guest.yaml
# (optional) write some in-VM state you want every clone to start from, then:
kubectl apply -f config/samples/clone-from-snapshot/03-snapshot.yaml
kubectl get swiftsnapshot clone-snap -w        # wait for Ready

kubectl apply -f config/samples/clone-from-snapshot/04-clone-pool.yaml
kubectl get swiftguest -l swift.kubeswift.io/pool-name=clone-pool -o wide -w
# each replica: Downloading (Tier C) → Restoring → Running, spread across nodes
```

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| Guest `Failed`, *"source SwiftGuest … no longer exists"* | The snapshot's source guest was deleted. cloneFromSnapshot needs the live source spec. |
| Guest `Failed`, *"requires spec.cloneFromSnapshot.targetNode"* | Tier C clone without a target node. In a pool the controller assigns it; a standalone clone must set it. |
| Guest stuck `Pending`, *"waiting for SwiftSnapshot … to be Ready"* | The snapshot is still Capturing/Uploading. |
| Guest stuck `Pending`, download Job present | The Tier C download is in progress (or `ImagePullBackOff` / `snapshot-s3 image not configured`). |
| All clones share hostname/machine-id | Expected on resume — reboot each clone once to fire the regen bootcmd. |

## Cluster walkthrough

Validated on the dev cluster (k0s 1.34, CH v51.1, in-cluster MinIO, miles+boba).
A `clone-source` (rocky9) on boba got a sentinel + a Tier C `clone-snap`
(Ready); a 2-replica `clone-pool` then cloned it. The walkthrough caught **two
real bugs** unit tests structurally cannot (the recurring **W5 pattern** —
fake-client tests verify control flow, not on-cluster runtime behavior); both
are fixed.

1. **The clone loaded `--restore` but stayed PAUSED.** The restore-receive
   launcher runs `CH --restore` (which loads the guest with vCPUs *stopped*) and
   reports `GuestRunning=True`, but nothing unpaused it — a cloneFromSnapshot
   guest has no SwiftRestore controller to drive a Resuming phase. On-cluster:
   `vm.info` `state=Paused`, console dead. **Fix:** the SwiftGuest controller
   sends the one-shot `kubeswift.io/snapshot-action: resume` to the clone's
   launcher pod once it is Running (idempotent), mirroring SwiftRestore.
2. **`guestClassRef` CRD-vs-webhook mismatch.** PR 2's webhook made
   `guestClassRef` optional for clones, but the CRD schema **requires** it — so
   the apiserver rejected the pool template before the webhook ran. **Fix:**
   require `guestClassRef` for every boot source (clones ignore it for resources
   but must set it).

Results after the fixes:

| | clone-pool-0 | clone-pool-1 |
|---|---|---|
| Assigned node | **boba** | **miles** (cross-node) |
| Tier C download Job | on boba ✓ | on miles ✓ |
| Phase | Running | Running |
| Hypervisor MAC | `52:54:00:7e:0c:47` | `52:54:00:fd:c6:1a` (distinct) |
| Sentinel `CLONE-POOL-…` | survived ✓ | survived ✓ |
| machine-id | inherited (resume-vs-boot) | inherited (resume-vs-boot) |

The 2-replica pool pre-assigned one replica per worker node, each ran its own
node-pinned download from object storage, booted as a clone via `CH --restore`,
resumed (one-shot resume action), and came up with the source's in-VM state
preserved and a distinct per-clone hypervisor MAC. Guest-visible identity
(machine-id / hostname / MAC) is inherited until each clone's first reboot — the
documented resume-vs-boot rule.
