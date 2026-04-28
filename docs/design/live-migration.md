# Live Migration — Design

> **Status:** Design — not yet implemented
> **Audience:** KubeSwift maintainers, AI assistants picking up this work
> **Last updated:** April 25, 2026
> **Target file path in repo:** `docs/design/live-migration.md`
>
> **Prerequisites:** read `docs/design/snapshots.md` first. This design assumes
> the snapshot infrastructure (CSI VolumeSnapshot integration, SwiftImage
> clone strategy, the unified clone PVC code path) exists. Live migration
> reuses several concepts from snapshots and shares some primitives, but is a
> distinct feature with its own CRDs and controllers.

---

## Purpose

This document describes how KubeSwift will support **live migration** of
virtual machines between Kubernetes nodes — moving a running VM from one
host to another with downtime measured in milliseconds rather than seconds,
without interrupting the guest workload.

It is split into:

1. **Concepts** — what we're building, the architecture, the constraints
2. **Work Plan** — phased implementation tasks ready to execute

When picking this up, read the Concepts section in full first. Live
migration touches the runtime, the controllers, the network model, the
storage model, and the scheduling model in interlocking ways. Skipping
concepts will lead to bad implementation choices.

---

# Part 1 — Concepts

## Goal

Enable KubeSwift operators to move a running SwiftGuest from one Kubernetes
node to another while the guest workload continues to operate, with
downtime targeting **under 300ms** (Cloud Hypervisor's default) for typical
VMs. Live migration must integrate cleanly with Kubernetes node drain,
pod scheduling, networking, and the existing SwiftGuest lifecycle, and
must work alongside (not replace) the offline migration that falls out of
the snapshot infrastructure for VMs that cannot be live-migrated.

## What Live Migration Means in KubeSwift

A live migration is a coordinated handoff of a running VM from a **source
launcher pod** on one node to a **destination launcher pod** on another
node. The guest workload remains operational throughout, with a brief
blackout (the **downtime window**) during the final state transfer.

The Kubernetes object identity stays the same: the SwiftGuest CR is
unchanged from the operator's perspective. The launcher pod underneath it
is replaced atomically.

The high-level flow:

```
SwiftMigration created (referencing a SwiftGuest, target node)
  → controller validates compatibility
  → controller creates destination launcher pod (paused, awaiting migration)
  → controller establishes secure channel between source and destination
  → controller initiates Cloud Hypervisor send-migration on source
  → CH pre-copy iterates: dirty pages copied repeatedly
  → CH converges or hits downtime target → final stop-and-copy
  → guest resumes on destination launcher pod
  → controller swaps SwiftGuest's podRef to point at destination
  → source launcher pod terminated
  → SwiftMigration.status.phase = Completed
```

## Hard Constraints (read this section twice)

These come from Cloud Hypervisor or from the underlying virtualization
stack. They cannot be designed around.

### Constraint 1 — VFIO devices block live migration

Cloud Hypervisor's live migration implementation does not support VFIO
passthrough devices. This has been an open upstream issue (#2251) for
years and remains true in v51.x.

Affected workloads (cannot be live migrated):

- All GPU VMs (Tier 1 PCIe, Tier 2 HGX shared, Tier 3 HGX full)
- All SR-IOV NIC passthrough VMs
- Any workload using vfio-pci

For these workloads, **offline migration** via the snapshot infrastructure
is the only option: stop the VM, capture disk snapshot, schedule on
destination node, restore. This produces seconds-to-minutes of downtime,
not milliseconds.

### Constraint 2 — Same Cloud Hypervisor version on both sides

"Live migration is not supported across different versions." This is
upstream's wording, not ours.

Operational implication: rolling out a new Cloud Hypervisor version
(packaged inside swiftletd) requires a careful upgrade sequence:

1. Stop creating new SwiftGuests until the rollout is complete (or accept
   they may be on the new version)
2. For each node, drain by evicting VMs to nodes still on the old version
3. Upgrade the node's swiftletd image
4. Allow new VMs to schedule there

KubeSwift's existing Helm chart upgrade path doesn't currently respect
this — operators upgrading swiftletd images today get a pod restart that
hard-stops the VM. Once live migration ships, the upgrade workflow needs
documentation that says "drain first, then upgrade" — or eventually,
controller-driven automation that handles it.

### Constraint 3 — virtio-fs is incompatible with live migration

If KubeSwift adds virtio-fs file sharing in the future (it doesn't today),
those VMs become non-live-migratable. This isn't a present blocker, just
something to flag in the doc so it doesn't sneak in unnoticed.

### Constraint 4 — Storage paths must match on both nodes

Cloud Hypervisor's live migration does not transfer the VM's disk content.
Both source and destination launcher pods must see the **same disk data**
through their respective filesystem paths — typically the same content
mounted at the same path, e.g. `/var/lib/kubeswift/disks/root/image.raw`.

This forces a key design decision: **how do we ensure the destination
node can read the same disk data?** Three options, only one of which works
broadly today:

- **Shared storage (RWX PVC)**: the root disk PVC is bound to a
  ReadWriteMany volume reachable from both nodes. CSI handles the
  attach/detach. Works clean. Not all CSI drivers offer RWX. Many
  performant block storage classes are RWO only.
- **Storage live migration (CSI clone in flight)**: stream the disk
  contents to the destination concurrently with memory pre-copy. Doable
  only with specific CSI features; complicated. Not v1 scope.
- **RWO with hand-off**: detach from source, attach to destination
  during the brief downtime window. Adds disk-attach latency to downtime;
  may push downtime above the 300ms target. Currently the most pragmatic
  path on the most common CSI drivers (Longhorn, Ceph RBD, EBS).

This design supports both the **shared storage** path (preferred when
available) and the **RWO hand-off** path (broadly applicable). Storage
live migration is out of scope for v1.

### Constraint 5 — CPU feature compatibility

The destination node's CPU must support all CPU features the VM is using.
Cloud Hypervisor's `--cpu` defaults to passing through the host CPU. Two
strategies:

- **Identical CPUs**: source and destination are the same model. This is
  the case for any homogeneous cluster (typical for Kubernetes node
  pools). Works trivially.
- **Baselined CPU model**: VMs are configured with a minimum-common CPU
  model (e.g., `--cpu boot=2,model=Haswell`) that all destination
  candidates support. KubeVirt's approach.

This design starts with **identical CPUs only** — node selection for
migration restricts to nodes with a matching CPU label. CPU baselining
is a future enhancement, not v1.

### Constraint 6 — Network must converge after migration

The VM keeps the same MAC and the same IP across the migration. On
KubeSwift's tap+bridge networking model, the destination launcher pod's
network-init must produce a tap with the same MAC as the source, and the
bridge must be on the same L2 broadcast domain (or a routed equivalent
that delivers the IP to the new node).

Two scenarios:

- **Same node-local bridge (single host migrations during testing)**:
  trivial; bridges are equivalent.
- **Cross-node migration (real case)**: requires that the guest's IP
  remain reachable. This is where the network model matters.

KubeSwift's default networking today is **node-local bridge with NAT
masquerade out the pod's eth0**. The guest IP is a private RFC1918 address
not reachable from outside the source node's pod. Migrating to a
different node breaks the IP unless the network is re-architected.

For live migration to work cross-node, the cluster must use a network
attachment that **carries the guest IP across nodes** — typically:

- **Multus + macvlan/bridge on a physical NIC** that's the same L2
  segment on both nodes
- **OVN-Kubernetes localnet or layer-2 secondary network** (logical
  switch spanning both nodes)
- **OVN-Kubernetes user-defined networks (UDN)** with the right scope

This makes live migration **opt-in via network attachment**: VMs on the
default node-local bridge cannot live migrate; VMs on a multi-node
network attachment can. This must be validated at SwiftMigration creation
time and surfaced clearly to operators.

## What Composes With What

This is a useful mental model. Live migration is fundamentally a
**superset** of offline migration, which is itself a superset of
snapshot/restore.

```
snapshot/restore       — pause, capture, resume (same VM, same node)
       ↓
offline migration      — pause, snapshot, restore on different node, resume
       ↓
live migration         — pre-copy memory, stop, copy delta, resume on different node
```

For VMs that **cannot** live-migrate (VFIO, virtio-fs, etc.), KubeSwift
falls back to offline migration cleanly. Operators get migration
functionality for **all** VMs; only the downtime characteristics differ.

This composition is intentional and shapes the design — the same
SwiftMigration CRD covers both modes, with the controller picking
live-vs-offline based on VM capabilities.

## Use Cases

### Use Case 1 — Node Drain for Maintenance

The most operationally important use case. Operator wants to take a node
out of service (kernel patching, hardware replacement, decommissioning)
without interrupting VMs running on it.

**Workflow:**

```
operator drains node:
  kubectl drain <node> --ignore-daemonsets

KubeSwift webhook intercepts pod eviction for swiftguest launcher pods:
  → for each affected SwiftGuest:
       create a SwiftMigration to move it to a healthy node
  → block eviction until migration completes (or fails)

migrations execute:
  → live where possible (pre-copy, ms downtime)
  → offline where not (snapshot+restore, seconds downtime)

drain completes when all VMs have migrated.
```

The eviction interception is the key integration point. Without it,
`kubectl drain` would just delete launcher pods and SwiftGuests would
fail and recreate elsewhere via runPolicy — which is restart, not
migration. Operators must be able to use standard Kubernetes operations
and have KubeSwift do the right thing.

### Use Case 2 — Operator-Initiated Migration

Operator wants to move a specific VM, typically for rebalancing,
relocation closer to data, or troubleshooting (move a VM off a problem
node before deciding what to do with the node).

**Workflow:**

```yaml
apiVersion: migration.kubeswift.io/v1alpha1
kind: SwiftMigration
metadata:
  name: db-vm-rebalance
spec:
  guestRef:
    name: database-vm
  target:
    nodeName: worker-3        # specific node OR
    nodeSelector:             # label-based scheduling
      role: database-host
  mode: auto                  # auto | live | offline
  downtimeTarget: 300ms       # Cloud Hypervisor downtime_ms
  timeout: 30m
  timeoutStrategy: cancel     # cancel | ignore
```

Submitted via `kubectl apply` or `swiftctl migrate <guest> --to <node>`.

### Use Case 3 — Automatic Rebalancing (Future)

Long-term: the cluster autonomously rebalances VMs across nodes based on
resource pressure, node health, or topology preferences. Not v1; mentioned
here to ensure the design doesn't preclude it. The SwiftMigration CRD as
specified can be created by either operators or future controllers.

### Use Case 4 — Disaster Response

A node is unhealthy but not yet failed. Operator wants to move VMs off
proactively. This is fundamentally Use Case 1 (drain), just initiated by
a different signal — health monitoring or a manual trigger rather than a
maintenance plan.

## CRD Design

### SwiftMigration

The new CRD lives in a new API group `migration.kubeswift.io/v1alpha1`.

```yaml
apiVersion: migration.kubeswift.io/v1alpha1
kind: SwiftMigration
metadata:
  name: db-vm-to-worker-3
  namespace: production
spec:
  guestRef:
    name: database-vm
  target:
    # exactly one of nodeName or nodeSelector
    nodeName: worker-3
    nodeSelector:
      role: database-host
  mode: auto                    # auto | live | offline
  downtimeTarget: 300ms         # CH downtime_ms; ignored for offline
  parallelConnections: 4        # CH connections; ignored for offline
  timeout: 30m
  timeoutStrategy: cancel       # cancel | ignore
  reason: "node-drain"          # informational; populated by drain webhook
status:
  phase: Pending | Validating | Preparing | PreCopy | StopAndCopy | Resuming | Completed | Failed | Cancelled
  conditions:
    - type: Ready
      status: "True"
    - type: Compatible
      status: "True"             # set during Validating phase
  mode: live                    # actual mode picked (relevant when spec.mode=auto)
  sourceNode: worker-1
  destinationNode: worker-3
  sourcePodRef:
    name: db-vm-source-pod
  destinationPodRef:
    name: db-vm-dest-pod
  startedAt: "2026-04-25T12:00:00Z"
  preCopyStartedAt: "2026-04-25T12:00:05Z"
  preCopyIterations: 7
  bytesTransferred: 4194304000
  totalMemoryBytes: 4294967296
  blackoutStartedAt: "2026-04-25T12:00:42Z"
  completedAt: "2026-04-25T12:00:42.298Z"
  observedDowntime: 287ms
  failureMessage: ""
```

### SwiftGuest extensions

Add a `migration` block to spec for default migration policy:

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
spec:
  migration:
    enabled: true              # if false, this VM is pinned and cannot migrate
    preferredMode: auto        # auto | live | offline (default auto)
    downtimeTarget: 300ms      # default if SwiftMigration doesn't override
```

If `migration.enabled` is false, drain webhook will not migrate the VM —
it'll either block drain (configurable) or allow the VM to terminate.
Useful for VMs where the operator has decided migration isn't safe.

The `migration` field is optional; default is `enabled: true, preferredMode:
auto`.

### Add `Ready` condition (per the GitOps design recommendation)

SwiftMigration exposes a `Ready` condition that turns True when
`phase: Completed`. This is what Flux and other tooling check.

## The Migration Mode Decision

When `spec.mode: auto`, the controller decides between live and offline:

```
Validating phase:
  if VM has VFIO devices (gpuProfileRef set, or SR-IOV interfaces):
    → mode = offline
  elif VM has virtio-fs (future):
    → mode = offline
  elif VM's network attachment is node-local-only:
    → if target node ≠ source node: mode = offline (network won't follow)
       else: mode = live (same-node migration is rare but valid)
  elif source CH version ≠ destination CH version:
    → mode = offline
  elif source and destination CPU models incompatible:
    → mode = offline
  else:
    → mode = live
```

Operators can force `mode: live` to get a clear failure (Compatible=False)
if their VM can't actually live migrate, rather than silently falling back
to offline.

## Architecture: How Migration Actually Happens

This is the most consequential section. Read carefully.

### The destination launcher pod

The destination is a **new launcher pod** scheduled on the target node.
It must come up in a **special "awaiting migration" mode** where:

- The pod runs (network-init prepares networking, swiftletd starts)
- swiftletd does NOT spawn Cloud Hypervisor as it normally would
- Instead, swiftletd starts CH with `--api-socket` and waits for a
  `receive-migration` API call

This "awaiting migration" mode is signaled via the RuntimeIntent:

```json
{
  "guestId": "default/database-vm",
  "cpu": 2,
  "memory": 4096,
  ...
  "migration": {
    "role": "destination",
    "listenAddress": "0.0.0.0:9999"
  }
}
```

When swiftletd sees `migration.role: destination`, it:

1. Starts Cloud Hypervisor with `--api-socket` only (no `--cpus`,
   `--memory`, etc. — those will come from the migration stream)
2. Calls `ch-remote receive-migration tcp:0.0.0.0:9999` to put CH in
   listening state
3. Reports readiness via pod annotation `kubeswift.io/migration-ready: true`
4. Waits for migration to complete; once VM is restored, swiftletd
   transitions to normal monitoring mode

### The source side

On the source, swiftletd already has a running CH instance with a known
API socket. The migration controller calls swiftletd's control surface
to issue `ch-remote send-migration destination_url=tcp:<dest>:9999`.

The source's swiftletd:

1. Receives the migration request (via annotation or HTTP — see
   "Control Surface" below)
2. Validates it's for this VM
3. Issues the send-migration with the configured downtime target,
   parallel connections, timeout, and timeout strategy
4. Streams progress (bytes transferred, iteration count) back via
   pod annotations
5. On completion, the source CH process exits cleanly; swiftletd
   reports completion via annotation
6. Source launcher pod is then terminated by the controller

### The secure channel

Cloud Hypervisor's TCP migration is **plaintext**. The doc explicitly
calls out "consider migrating in a trusted network." We cannot rely on
plaintext over a generic Kubernetes pod network — even on a private
cluster, a snooping pod could read VM memory in flight.

KubeSwift's solution: **the migration traffic flows through a Kubernetes
Service that targets only the destination launcher pod, and the source
launcher pod connects through a stunnel/socat sidecar that does mTLS**.

```
source launcher pod                     destination launcher pod
  ┌──────────────┐                       ┌──────────────┐
  │   swiftletd  │                       │  swiftletd   │
  │      ↓       │                       │      ↑       │
  │  CH (source) │                       │  CH (dest)   │
  │      ↓       │                       │      ↑       │
  │  stunnel     │ ──── mTLS ───→        │  stunnel     │
  │  (client)    │   over Service IP     │  (server)    │
  └──────────────┘                       └──────────────┘
```

Implementation details:

- The migration controller generates an ephemeral certificate pair for
  each migration, signed by a controller-managed CA
- Certificates are mounted into both launcher pods via Kubernetes Secrets
- stunnel (or socat with OpenSSL — TBD spike) wraps the local TCP
  connection
- The destination's stunnel service exposes a Kubernetes Service so the
  source pod can resolve it cluster-internally
- mTLS prevents both eavesdropping and unauthorized migration sources

This adds two sidecar processes per migration but is the only way to
make this safe by default. In a fully trusted cluster (homelab), an
operator could disable mTLS via a controller flag — but that should not
be the default.

### The control surface for swiftletd

swiftletd needs to receive instructions from the controller for several
operations: start migration, prepare for receive, report progress. The
existing pattern in KubeSwift is **annotation-driven**: controller
writes an annotation, swiftletd reacts.

For migration, this works:

- Controller sets annotation `kubeswift.io/migration-action: send`
  with parameters in `kubeswift.io/migration-params` (JSON)
- swiftletd watches its own pod, reads the annotation, performs the
  action, writes results back as annotations (`migration-status`,
  `migration-bytes-transferred`, etc.)
- Controller reconciles the SwiftMigration based on these annotations

This composes with the existing `kubeswift.io/guest-ip`,
`kubeswift.io/guest-runtime-pid` patterns.

Alternative (HTTP control surface) was considered and rejected for
adding more attack surface and complicating mTLS for the swiftletd
itself.

### The handoff: from source pod to destination pod

The crucial moment. When CH on source exits cleanly after migration:

1. Source swiftletd reports `migration-status: source-completed`
2. Destination swiftletd reports `migration-status: destination-running`
3. Controller updates SwiftGuest:
   - `status.podRef` → destination pod
   - `status.nodeName` → destination node
   - `status.network.primaryIP` is **expected to be unchanged** (the
     guest didn't change its IP)
4. Controller terminates source pod

There is a brief window where both pods exist. SwiftGuest's `podRef`
must be updated atomically and the controller must NOT delete the
source pod until the destination is confirmed running and serving
traffic. This is enforced by the state machine: source-pod-deletion
only happens after `phase: Completed`.

### What the operator sees

From `kubectl get swiftguest`:

```
NAME           PHASE     IP              NODE       AGE
database-vm    Running   10.244.5.42     worker-3   12h
```

Before migration: `NODE = worker-1`. After migration: `NODE = worker-3`.
The IP is unchanged, the SwiftGuest's age is unchanged, the conditions
are unchanged. The migration looks like a node attribute change.

The SwiftMigration resource shows the operation history:

```
NAME                  GUEST          MODE   FROM       TO         DOWNTIME    AGE
db-vm-to-worker-3     database-vm    live   worker-1   worker-3   287ms      5m
```

## Pre-copy Mechanics

Cloud Hypervisor uses **pre-copy** migration, which is industry-standard.

### How pre-copy works

1. **Iteration 0**: copy all VM memory to destination, while VM continues
   to run on source. Memory is large, so this takes time. During this
   time, the VM dirties some pages.
2. **Iteration 1**: re-copy only pages dirtied during iteration 0.
   Smaller. The VM dirties some pages again.
3. **Iteration N**: re-copy dirtied pages. Each iteration is smaller as
   the dirty rate stabilizes — IF the dirty rate is less than the
   transfer rate.
4. **Stop-and-copy**: when remaining dirty pages can be transferred
   within the `downtime_ms` budget, pause the VM, transfer the final
   delta, resume on destination.

If the dirty rate exceeds the transfer rate (e.g., a memory-intensive
workload), pre-copy never converges. The `timeout_strategy` controls
behavior:

- `cancel` (default): abort migration when timeout is hit, VM keeps
  running on source
- `ignore`: proceed with stop-and-copy regardless of how much memory is
  still dirty — this can produce significant downtime (seconds)

### When pre-copy fails to converge

This is real and not rare. Memory-pressure workloads (Redis under heavy
write, in-memory databases doing bulk loads) can sustain dirty rates
faster than network transfer rates.

KubeSwift's response:

- Default `timeout_strategy: cancel` — operators discover their VM is
  unmigratable and can decide what to do
- Surface this clearly: `SwiftMigration.status.failureMessage` says
  "pre-copy did not converge within timeout; consider higher
  parallelConnections, longer timeout, or offline migration"
- Document workload characteristics that cause this (large in-memory
  caches under heavy write load) so operators can plan

Operators who absolutely must migrate such VMs can:

1. Increase `parallelConnections` (up to 128) to widen the transfer pipe
2. Increase `timeout` to allow more pre-copy iterations
3. Switch to `mode: offline` for guaranteed completion at higher downtime
4. Consider quiescing the workload temporarily (application-level)

### Bandwidth is the constraint

Migration speed is bounded by network bandwidth between nodes.
On a 1Gbps cluster network, transferring 32GB of memory takes ~5 minutes
optimal. On 25Gbps, it's ~10 seconds. Memory-heavy VMs need fast cluster
networks for live migration to be practical.

This has a network architecture implication: large VMs benefit hugely
from a **dedicated migration network** (a second NIC bonded into a
separate VLAN), but that's a cluster-design choice, not a KubeSwift
feature. We document the recommendation; we don't enforce it.

## Storage Hand-off

### Shared storage path (preferred)

When the root disk PVC has access mode `ReadWriteMany`:

- Both source and destination pods can mount it simultaneously
- Cloud Hypervisor on destination opens the same disk file
- No detach/attach during downtime
- Disk content is consistent because the source CH has fsync'd before pause

This is the cleanest path. Works with: cephfs, NFS-backed CSI, Longhorn
(in RWX mode), portworx, etc. May not work with all block-CSI drivers.

### RWO hand-off path

When the disk PVC is `ReadWriteOnce`:

The migration sequence becomes:

```
PreCopy phase:
  → memory pre-copy iterations on source
  → destination pod has the PVC volume mount declared but pod not started
    OR pod started in init phase waiting on a marker
  → at iteration convergence: source pauses CH

StopAndCopy phase:
  → controller initiates PVC reattach:
       - request PVC unmount/detach from source node
       - wait for detach completion
       - request PVC attach to destination node
       - wait for attach completion
  → controller signals destination launcher to enter receive-migration
  → CH receive-migration completes (final delta + resume)
```

This adds the **detach+attach time** to downtime. On EBS or Ceph this
is typically 5-15 seconds — well above the 300ms target. The result:
"live migration" with RWO storage produces seconds of downtime, not
milliseconds. Still better than offline migration's full
stop-snapshot-restore (which is tens of seconds to minutes), and the VM
process state is preserved.

The doc should be honest: **with RWO storage, "live migration" downtime
is bounded by storage detach-attach speed**, not by Cloud Hypervisor's
default 300ms.

### What about data disks?

If the SwiftGuest has `dataDiskRefs`, all of those PVCs need to be
handled the same way. Mixed access modes (root RWX, data RWO) work fine
— the controller handles each disk per its access mode.

### What about per-guest root disk clones?

Per the snapshot doc, KubeSwift creates a `swiftguest-root-<name>` clone
PVC for each SwiftGuest. The clone PVC is owned by the SwiftGuest. For
live migration, this PVC follows the same shared-storage / RWO-handoff
logic above, depending on its storage class.

### Storage class compatibility

The destination node must be able to attach the disk PVC. For RWO with
hand-off, this means the storage class must support cross-node access
(EBS, Ceph RBD, Longhorn). Local-path-provisioner does NOT — its volumes
are physically tied to one node and cannot migrate.

**Validation**: at SwiftMigration creation, the controller checks the
PVC's storage class and the destination node's compatibility. Local-only
storage classes refuse migration with a clear error.

## Networking Hand-off

### MAC preservation

The guest's MAC address is part of CH's migrated state. After migration,
the guest's NIC retains the same MAC. The destination launcher pod must
create a tap interface bound into a bridge with the same MAC behavior.

network-init currently generates a deterministic MAC from the SwiftGuest
name. For migration, the destination's network-init must use the *same*
MAC the source had (which equals the deterministic-from-name MAC, so they
match by construction — confirm in spike).

### IP preservation

The guest's IP is in the guest's network stack — it's whatever the guest
believed before migration. The host has no say. After migration:

- Guest still believes it has IP X
- Source bridge no longer routes X
- Destination bridge must route X

This is the **network constraint** from earlier. For cross-node
migration to preserve IP:

1. Both nodes must be on the same L2 segment (multus + bridge or
   macvlan on a shared physical NIC), OR
2. The cluster must use a layer-2 secondary network (OVN-K localnet,
   layer-2 UDN) that spans both nodes, OR
3. The guest must have an IP in a routed network with mobility support
   (e.g., a /32 route advertised by the destination after migration)

KubeSwift's default node-local bridge does NOT support cross-node IP
preservation. Live migration on the default network is **same-node only**
(useful for in-place CH version upgrades, not for node drain).

For real cross-node live migration, operators must use multi-node
network attachments. This is a configuration constraint we document
prominently.

### ARP / NDP gratuitous announcements

After migration, the guest's MAC is now reachable through the destination
node. Any neighbor caches that learned the source-side path are stale.
Gratuitous ARP/NDP announcements clear them.

Cloud Hypervisor itself doesn't do this — it's a guest OS concern. Most
modern Linux kernels do gratuitous ARP on link-state-up events. The
network-init script could trigger a gratuitous ARP from the destination
node side as a backup.

### Multi-NIC and network attachment hand-off

For multi-NIC VMs, **each interface must be migratable**. If any one
NIC is on a node-local-only network (e.g., the management network is
shared but the data network is node-local), the whole VM cannot
cross-node migrate.

The controller validates: every interface on the SwiftGuest must be on
a network attachment that the destination node can also attach to. CRD
validation in the SwiftMigration's Validating phase catches this.

## Drain Integration

This is what makes migration operationally valuable. Without drain
integration, operators have to manually issue migrations before draining
— too easy to miss VMs and lose them.

### Approach: ValidatingAdmissionWebhook on Pod evictions

Kubernetes evictions go through the Eviction API
(`kubectl drain` issues these). KubeSwift registers a
ValidatingAdmissionWebhook that intercepts evictions of pods labeled
`kubeswift.io/launcher-pod`.

When the webhook receives an eviction:

1. Look up the SwiftGuest owning the pod
2. If `spec.migration.enabled: false`, allow eviction (operator opted
   out — VM will fail and recreate via runPolicy)
3. Otherwise: deny eviction with a message explaining migration is in
   progress, and asynchronously create a SwiftMigration targeting any
   healthy node
4. When migration completes, the source pod terminates naturally —
   subsequent evictions of that (now non-existent) pod just succeed
5. `kubectl drain` retries evictions on its standard interval until they
   succeed

The webhook denial pattern is standard for "operation in progress, retry
later" — Kubernetes drain handles this gracefully (continues retrying).

### Edge cases

- **Multiple drains in parallel**: rare but possible. The controller
  must not create duplicate SwiftMigrations for the same SwiftGuest.
  Use a finalizer or a deduplication key.
- **Drain during ongoing migration**: pod is already migrating. Eviction
  is denied; drain retries; migration completes; new launcher pod
  exists on a different node; original pod is gone; subsequent evictions
  succeed.
- **No healthy target nodes**: migration creation succeeds (Pending),
  fails validation (no target available), reports failure. Drain
  retries; eviction stays denied. Operator must manually intervene
  (add capacity, force-delete the pod).
- **Migration failure during drain**: SwiftMigration fails. The original
  pod remains running. Drain stays blocked on this pod. Operator must
  decide: investigate, retry, force-delete.

### `kubectl drain --force` behavior

`--force` deletes pods even if they would otherwise be denied. This
bypasses the migration webhook entirely. The VM dies. This is the
correct behavior — `--force` means "I know what I'm doing, delete it
anyway." Document it clearly so operators don't surprise themselves.

## Failure Modes and Rollback

Live migration can fail at many points. The state machine must handle
all of them safely.

### Pre-copy timeout (mode: cancel)

Most common failure. Source CH aborts migration; VM keeps running on
source. Destination pod is terminated. SwiftMigration phase=Failed,
SwiftGuest unaffected.

**Rollback**: nothing to do; source is canonical.

### Network failure during migration

TCP connection drops between source and destination. CH's behavior:
both sides detect the failure, source aborts, destination terminates.

**Rollback**: source pod still has the running VM. Destination pod is
cleaned up. SwiftMigration phase=Failed.

### Source pod dies during migration

The launcher pod gets killed by something (OOM, node failure). CH dies
with it. The migration is already in progress — what state is the
destination in?

- If source died during pre-copy: destination CH is in receive-migration
  mode with partial state. Cleanup: terminate destination pod.
- If source died during stop-and-copy (after pause but before delta
  transfer complete): the VM state is "lost in flight." This is the
  scariest case.

The safe response: SwiftMigration phase=Failed,
SwiftGuest goes through normal failure handling per its runPolicy
(Always = restart, RestartOnFailure = restart, Never = stop). The VM
"resurrects" from its disk state, like a power-loss recovery. Memory
state is lost.

### Destination pod fails to schedule

Target node has insufficient resources, or PVC can't be attached, or
some other Kubernetes-level scheduling issue. SwiftMigration stays in
Preparing phase, eventually times out.

**Rollback**: clean up any partial destination resources. SwiftMigration
phase=Failed. Source unaffected.

### Storage detach fails (RWO path)

The PVC won't unmount from the source node. Common cause: kernel I/O
hung. The destination pod can't attach. The VM was paused for migration
but now there's no path forward.

**Rollback**: re-attempt detach with timeout. If still fails, **resume
the source VM** to its pre-migration state (if its CH process is still
alive — it should be, since it's just paused). Mark SwiftMigration
failed.

This is where having the controller hold the state machine carefully is
critical. Source CH is paused, not killed. Resume is the rollback.

### Destination CPU rejects guest CPU features

Already supposed to be caught in Validating phase, but if the validation
was wrong, CH on destination will fail to restore. Surface as Failed,
clean up destination pod, source resumes (it's still paused at this
point).

### State machine summary

```
Pending → Validating → [if invalid] Failed
              ↓
         Preparing → [destination pod ready] PreCopy
              ↓
         PreCopy → [converged] StopAndCopy
              ↓
         StopAndCopy → [delta sent] Resuming
              ↓
         Resuming → [destination running] Completed

at any phase: → Failed (cleanup destination, resume source if paused,
                       emit event with reason)
              → Cancelled (operator deleted SwiftMigration; same cleanup)
```

The state machine is **enforced by the controller**. Phase transitions
are atomic. Annotations from swiftletd update sub-fields (bytes
transferred, iteration count) but do not advance the phase — only the
controller advances phase based on confirmed evidence.

## Lifecycle Interactions

### With SwiftGuestPool

Pools schedule replicas; live migration moves them between nodes. They
must work together cleanly.

- Pool members can be live-migrated. Each pool member's SwiftGuest is
  individually migratable.
- Pool's `spec.template.spec.migration` defaults apply to all members.
- Pool topology spread: after migration, the new node may violate the
  pool's `topologySpreadConstraints`. The pool controller must NOT
  immediately try to "rebalance" the migrated VM back — that would
  defeat the migration. Instead, topology drift is accepted as a
  trade-off of migration. The pool controller's spread evaluation
  applies only at creation time and during voluntary rolling updates,
  not as a continuous active enforcement.

### With SwiftGuestPool rolling updates

Rolling updates create new pool members and delete old ones. They are
**not** the same operation as migration — rolling update changes the
template (new image, new resources); migration moves an unchanged VM.

If a rolling update is in progress on a pool, and a node drain causes
migrations of pool members on the draining node:

- Members on the draining node are migrated (live or offline)
- Migrated members keep the OLD template (they're the same VM, just on
  a different node)
- The rolling update continues independently, replacing OLD members
  (anywhere they live) with NEW members
- Net effect: drain and rolling update compose without conflict

### With SwiftSnapshot and SwiftRestore

Live migration shares mechanics with snapshot/restore but is a distinct
flow. A SwiftMigration in progress should block:

- SwiftSnapshot creation against the same SwiftGuest (would race with
  migration's own pause)
- SwiftRestore that targets the same SwiftGuest name

These mutual exclusions are enforced by the migration controller's
validating webhook.

### With root disk cloning

The per-guest clone PVC migrates with the VM (Constraint 4 — same disk
path on both sides). Whether it's a snapshot-derived CoW PVC or a
copy-derived PVC, the migration logic is the same — it's just a PVC.

### With observability and metrics

New Prometheus metrics:

- `kubeswift_migration_total{phase, mode, result}` — counter
- `kubeswift_migration_duration_seconds{mode}` — histogram (full op time)
- `kubeswift_migration_downtime_seconds{mode}` — histogram (the
  blackout, what operators care about most)
- `kubeswift_migration_precopy_iterations` — histogram
- `kubeswift_migration_bytes_transferred` — histogram
- `kubeswift_migration_in_flight` — gauge (current count of active
  migrations, for capacity planning)

## What Is Explicitly Out of Scope

Listed so future work doesn't get confused:

- **Storage live migration** — moving the disk between storage classes
  during live migration. Out of scope for v1.
- **Post-copy migration** — the alternative pre-copy strategy where the
  VM resumes on destination before all memory has transferred. Cloud
  Hypervisor does not support this; it's a CH upstream concern.
- **CPU baselining** — automatic computation of a minimum-common CPU
  model. Out of scope for v1; identical-CPU clusters only.
- **Live migration of QEMU-backed VMs** — for now, anything on the QEMU
  runtime path (Tier 2/3 GPU, future virtio-fs) cannot live migrate.
  Future enhancement could add QEMU live migration via QMP — significant
  separate work.
- **Cross-cluster migration** — moving a VM from one Kubernetes cluster
  to another. Operationally requires cross-cluster networking, storage
  replication, etc. Far out.
- **Automatic eviction-based load balancing** — the cluster autonomously
  rebalancing VMs. Use Case 3 future work.
- **Live migration of VMs with attached PCI hotplug devices added at
  runtime** — even if the device isn't VFIO. State migration of
  hotplugged devices is upstream complexity.

## Trade-offs and Open Questions

**Downtime is a budget, not a guarantee.** The 300ms target is what
Cloud Hypervisor aims for. Real downtime depends on workload memory
churn, network bandwidth, and (for RWO storage) disk hand-off speed.
Operators will see real downtime measurements in
`SwiftMigration.status.observedDowntime`.

**The mTLS sidecar adds two processes per migration.** This is overhead
that doesn't exist today. For a cluster doing many parallel migrations
(node drain), this matters. We should size the controller accordingly.

**Drain webhook is a single point of failure.** If the webhook is down,
drains either fail-closed (drain blocked) or fail-open (VMs killed). We
must pick a policy and document it. Recommendation: fail-closed —
better to make the operator notice than to silently lose VMs.

**Open question: disk consistency during shared-storage migration.**
With RWX storage, both pods can write to the same file simultaneously
during the migration window. This is a correctness concern — CH on
source and CH on destination must not both write to the disk file at
the same time. Cloud Hypervisor's migration protocol coordinates this,
but we need to verify in the spike.

**Open question: how to express "live migration capable" to schedulers?**
Future: a SwiftGuest's ability to live migrate could be a scheduling
hint — the cluster autoscaler could prefer node-drain candidates that
have only live-migratable VMs. Not v1.

**Open question: migration of pool members during pool deletion.**
If a pool is being deleted, do we still migrate its members during a
drain? Probably not — they're going to be deleted anyway. But this
needs explicit handling so a drain in progress doesn't fight pool
deletion. Recommendation: skip migration if the SwiftGuest is being
deleted (has DeletionTimestamp).

---

# Part 2 — Phased Work Plan

The work plan delivers value incrementally. Earlier phases must work
before later phases build on them. Live migration is large enough that
even a minimal-feature first ship is meaningful.

## Phase 0 — Spike and validation (no production code)

**Goal:** prove the Cloud Hypervisor live migration cycle works
end-to-end manually on KubeSwift-style infrastructure, surface the
detailed constraints, time the operations, and confirm assumptions in
this design doc.

**Tasks:**

1. On a two-node dev cluster:
   - Run two CH instances manually, one on each node
   - Source: boot a small Linux VM (no GPU, simple disk, virtio-net)
   - Verify same kernel/disk paths on both nodes (rsync the disk file
     before testing)
   - Issue `ch-remote send-migration` with TCP transport
   - Issue `ch-remote receive-migration` on destination
   - Use socat or stunnel to wrap the connection
   - Time the entire operation, observe the downtime window
2. Test with various memory sizes (1G, 4G, 16G) to characterize the
   pre-copy convergence behavior
3. Test with intentionally heavy memory churn (a `dd if=/dev/zero
   of=/dev/shm/...` running in the guest) to see pre-copy fail to
   converge with `cancel`, succeed with `ignore`
4. Test the failure modes:
   - Kill source CH mid-migration — what happens to destination?
   - Network failure (iptables drop) mid-migration
   - CPU mismatch (artificial — pin source to features destination
     can't support)
5. Test with shared storage (CephFS) and confirm both CH instances can
   open the disk simultaneously without corruption when properly
   coordinated by CH's own migration protocol
6. Test with RWO storage (Ceph RBD) — manual detach/attach sequence
   to time the storage hand-off contribution to downtime
7. Test with multus + macvlan to confirm IP preservation across nodes
8. Document everything in `docs/design/live-migration-spike-results.md`

**Deliverable:** spike results document with timing measurements,
failure mode behaviors, and any deviations from this design doc that
need reconciling.

**Out of scope:** writing controller code, defining CRDs, integrating
with KubeSwift.

## Phase 1 — SwiftMigration CRD + offline migration only

**Goal:** ship the SwiftMigration CRD and controller with **offline
migration only**. This proves the end-to-end orchestration without the
deepest hypervisor work, and immediately delivers value for VFIO/SR-IOV
workloads that can never live migrate.

**Tasks:**

1. Define `SwiftMigration` CRD in `api/migration/v1alpha1/`. Implement
   the full spec/status from the Concepts section, but the controller
   only handles `mode: offline` (and `mode: auto` always picks offline
   for now).
2. Add `Ready` condition.
3. Implement `internal/controller/swiftmigration/` controller with
   offline mode using **direct PVC reuse** (NOT snapshot+restore):
   - Validating phase: re-resolve guest + class (defense in depth);
     stamp source/destination/mode on status; **manual capacity
     check** on target node (read Allocatable, sum running pod
     requests, compare against guest needs + LauncherMemoryOverhead).
     The webhook (rule set) catches submission-time errors; the
     controller catches transient cluster-state issues.
   - Preparing phase: write `kubeswift.io/migration-in-progress`
     annotation on the SwiftGuest as the idempotency marker
     (Risk 3); patch `runPolicy=Stopped` in the same combined
     `client.MergeFrom` so the SwiftGuest controller cannot observe
     a half-claimed state; `Delete(pod)` with grace=30s; **dual-
     poll** for Pod NotFound AND no VolumeAttachment for the per-
     guest root PV (the second gate is critical — without it the
     destination pod hits Multi-Attach errors on RWO storage; the
     Phase 1 spike measured ~13s gap on Longhorn).
   - StopAndCopy phase: **single combined `client.MergeFrom` patch**
     of `runPolicy=Running` AND `nodeName=target` on the same
     SwiftGuest CR (Approach A from the spike; the SwiftGuest CR
     identity is unchanged across the migration, only these two
     fields are toggled). Atomicity matters — split patches race
     the SwiftGuest controller's reconciler. Then poll for the
     destination launcher pod to appear pinned to target.
   - Resuming phase: poll for `GuestRunning=True` on the destination
     SwiftGuest; compute `observedDowntime` from Preparing entry to
     GuestRunning.
   - Completed phase: clear the in-progress annotation; set
     `Ready=True`.
   - Failure handling: drive forward post-cutover (architect Risk 2).
     Once `Delete(pod)` runs, never roll back to source — the
     migration is committed. Pre-cutover failures (Validating,
     Preparing-before-Delete) are pure rollbacks (annotation
     cleared, source untouched).

   The original Phase 1 plan called for snapshot+restore reuse
   (creating an internal SwiftSnapshot, hydrating a new SwiftGuest
   on the target). That plan was overridden after the Phase 1 spike
   showed direct PVC reuse via in-place SwiftGuest patch is simpler
   (no PVC ownerRef transition; the per-guest PVC stays owned by
   the same SwiftGuest UID throughout) and forward-compatible with
   Phase 3 live mode (which will also patch SwiftGuest fields rather
   than recreate the CR). See
   `docs/design/live-migration-phase-1-spike.md` for the empirical
   findings that drove the change.
4. SwiftGuest extensions: add `spec.migration` block (`enabled`,
   `preferredMode`) and `spec.nodeName`. The pod builder honors
   `spec.nodeName` via direct `pod.Spec.NodeName` binding (bypasses
   the scheduler — gives fast kubelet rejection on bad fits). When
   `spec.NodeName` and `status.GPU.NodeName` are both set, they MUST
   agree (the validation webhook enforces this; the pod builder
   refuses to build with a Resolved=False condition on disagreement).
5. swiftctl: `swiftctl migrate <guest> --to <node>`,
   `swiftctl migration list`, `swiftctl migration describe`
6. RBAC: SwiftMigration permissions; controller needs SwiftGuest
   patch + Pod delete + Node get + VolumeAttachment list
7. Helm chart updates
8. Tests:
   - Unit tests for each phase handler
   - e2e test in `test/migration/`: create a SwiftGuest, migrate to
     another node, verify the sentinel disk content survives
   - VFIO migration test: webhook REJECTS cross-node GPU migration
     (Phase 1 has no release-and-reallocate primitive)
   - Failure mode test: kill destination during preparation, verify
     source can be resumed
9. Documentation:
   - `docs/migration/overview.md` — concepts, modes, when to use each
   - `docs/migration/offline-migration.md` — offline migration deep
     dive with the spike's measured timing table for Longhorn vs.
     true CoW drivers
   - `docs/migration/networking-requirements.md` — storage class +
     network attachment requirements for cross-node migration
   - `docs/migration/troubleshooting.md` — common issues
   - swiftctl reference

**Deliverable:** operators can move SwiftGuests between nodes via
SwiftMigration. Downtime is bounded by storage detach + VM boot:
~70s on Longhorn full-copy, ~25s on true CoW drivers (Rook Ceph
RBD, EBS). Works for non-VFIO workloads cross-node; VFIO/SR-IOV
guests are rejected by the webhook (Phase 4+ work — no release-
and-reallocate primitive yet).

**Acceptance:**
- e2e test passes
- VFIO VM successfully migrates (offline)
- SwiftMigration UX works via swiftctl
- Documentation complete

## Phase 2 — swiftletd live migration plumbing

**Goal:** implement the swiftletd-side mechanics for live migration:
destination "awaiting migration" mode, source migration sending,
annotation-based control, progress reporting. **No drain integration
yet, no mTLS yet** — connect plaintext, manually trigger.

**Tasks:**

1. swiftletd RuntimeIntent extension: `migration.role`,
   `migration.listenAddress`, `migration.targetURL`
2. swiftletd "destination" mode: spawn CH with `--api-socket` only,
   issue `receive-migration`, wait
3. swiftletd "source" send-migration support: receive command via
   annotation, issue CH `send-migration`, monitor progress
4. Progress reporting via pod annotations:
   `kubeswift.io/migration-status`,
   `kubeswift.io/migration-bytes-transferred`,
   `kubeswift.io/migration-precopy-iterations`,
   `kubeswift.io/migration-observed-downtime`
5. Rust-side: extend `swift-ch-client` with send/receive migration
   API calls
6. Manual e2e: spin up two SwiftGuests-as-launcher-pods manually,
   trigger migration via kubectl annotate, observe handoff
7. NO controller integration yet — the goal is to prove the swiftletd
   plumbing in isolation

**Deliverable:** swiftletd can send and receive Cloud Hypervisor live
migrations. Manual end-to-end demonstration on a real cluster.

**Acceptance:**
- Manual migration succeeds with downtime <1s on a 4G VM with shared
  storage
- All progress annotations are populated
- Failure cases (network drop, source kill) leave the system in a
  recoverable state

## Phase 3 — SwiftMigration live mode + secure channel

**Goal:** wire swiftletd's live migration into the SwiftMigration
controller. Add mTLS for the migration channel. This is the headline
"live migration works" milestone.

**Tasks:**

1. Extend SwiftMigration controller with `mode: live` support
2. Implement the destination pod creation: same launcher pod template
   but with `RuntimeIntent.migration.role = destination`
3. mTLS: ephemeral cert generation per migration, controller-managed
   CA, certs mounted as Secrets into both pods, stunnel sidecars
   handle the encrypted channel
4. State machine implementation: full Validating → Preparing → PreCopy
   → StopAndCopy → Resuming → Completed flow
5. Auto-mode decision logic: pick live vs offline based on VM
   capabilities, network attachment, CH version match, CPU compatibility
6. Storage hand-off:
   - Shared storage path (RWX): both pods mount same PVC
   - RWO path: detach/attach orchestration during StopAndCopy
   - Validation refuses live migration on storage classes that don't
     support cross-node attach
7. Rollback: on failure during StopAndCopy or earlier, resume source
   CH (it's still paused), clean up destination pod
8. Observability metrics
9. swiftctl: support `--live` and `--offline` flags on `swiftctl
   migrate`
10. e2e tests:
    - Live migration with shared storage (downtime target <500ms)
    - Live migration with RWO storage (downtime target <30s)
    - Failure rollback (network drop during pre-copy → source still
      runs)
    - Mode auto-selection: GPU VM falls back to offline cleanly
11. Documentation:
    - `docs/migration/live-migration.md` — deep dive
    - `docs/migration/networking.md` — network attachment requirements
      for cross-node migration
    - `docs/migration/storage.md` — RWX vs RWO trade-offs
    - Performance characterization with measurements

**Deliverable:** operators can live migrate non-passthrough SwiftGuests
between nodes with sub-second downtime on shared storage, low-tens-of-
seconds downtime on RWO storage. mTLS-secured channel. Full state
machine including failure rollback.

**Acceptance:**
- Live migration on shared storage achieves <500ms downtime for a 4G VM
- Live migration on RWO storage achieves <30s downtime
- All e2e tests pass
- Auto-mode correctly picks offline for VFIO VMs

## Phase 4 — Drain integration

**Goal:** make `kubectl drain` automatically migrate KubeSwift VMs.

**Tasks:**

1. Implement ValidatingAdmissionWebhook for Pod evictions
   (`/v1/eviction` subresource)
2. Webhook logic: intercept evictions of pods labeled
   `kubeswift.io/launcher-pod`, look up SwiftGuest, create
   SwiftMigration if eligible, deny the eviction (drain will retry)
3. Deduplication: don't create duplicate SwiftMigrations for the same
   SwiftGuest during parallel evictions
4. Honor `spec.migration.enabled: false` (allow eviction, accept VM
   loss)
5. Edge cases: pods being deleted, drains during ongoing migrations,
   no healthy target nodes
6. Webhook failure policy: `Fail` (drain blocked when webhook is
   unreachable — safer than silently killing VMs)
7. e2e test: drain a node with running SwiftGuests, verify they
   migrate, verify drain completes
8. Documentation:
   - `docs/migration/node-drain.md` — operator guide for safe drains
   - Update README

**Deliverable:** node maintenance workflows just work. `kubectl drain`
behaves correctly for KubeSwift VMs.

**Acceptance:**
- Drain test: drain a node with 3 SwiftGuests (mix of types), verify
  each migrates correctly, drain completes within timeout
- Force-drain (`--force --delete-emptydir-data`) bypasses webhook
  cleanly (VM dies, doesn't deadlock)

## Phase 5 — Operational polish

**Goal:** observability, CLI ergonomics, edge case handling for
production readiness.

**Tasks:**

1. swiftctl polish:
   - `swiftctl migrate` interactive mode (prompts for target)
   - `swiftctl drain <node>` wrapper that surfaces migration status
   - `swiftctl migration cancel <name>`
2. Prometheus / Grafana:
   - Sample dashboard for migration operations
   - PrometheusRule alerts: migrations failing rate, average downtime
     trending up, pre-copy convergence rate dropping
3. Multi-migration coordination:
   - Limit on concurrent migrations cluster-wide (a controller flag,
     bounded resource use)
   - Priority handling for drain-initiated migrations vs operator-
     initiated
4. CH version skew handling:
   - Document the upgrade workflow
   - Surface version mismatches clearly in SwiftMigration validation
5. Documentation review pass with an operator unfamiliar with the design

**Deliverable:** live migration is operationally polished, well-
observable, and documented to a production quality bar.

## Phase Sequencing and Dependencies

```
Phase 0 (spike) — must come first, cannot be skipped
    ↓
Phase 1 (offline migration via SwiftMigration) — independently shippable
    ↓
Phase 2 (swiftletd live migration plumbing) — independent of Phase 1
    ↓                                       ↑
    ↓                                       │
Phase 3 (SwiftMigration live mode) — depends on Phase 1 + Phase 2
    ↓
Phase 4 (drain integration) — depends on Phase 3 (or even Phase 1)
    ↓
Phase 5 (polish) — final
```

**Phase 1 is independently shippable** — offline migration via
SwiftMigration is a real, useful feature that delivers operator value
even before live migration arrives. VFIO/SR-IOV operators benefit from
it day one and forever (those workloads can never live migrate).

**Phase 2 can run in parallel with Phase 1** — different code paths,
different teams could split.

**Phase 4 (drain) could ship after Phase 1** — if we want operators to
get safe drain immediately, even with the longer offline migration
downtime. The webhook is the same; only the migration mode differs.

## Estimated Effort

Rough sizing for one contributor familiar with KubeSwift:

| Phase | Effort |
|-------|--------|
| 0 — Spike | 5–7 days |
| 1 — Offline migration via SwiftMigration | 8–12 days |
| 2 — swiftletd live migration plumbing | 7–10 days |
| 3 — SwiftMigration live mode + mTLS | 12–18 days |
| 4 — Drain integration | 5–7 days |
| 5 — Polish | 4–5 days |

Total: roughly 8–12 weeks of focused work — by far the largest design
on the roadmap. Live migration is not a small feature.

Phase 1 alone takes about 2 weeks and delivers offline migration. Phase
3 is the largest single phase (live migration with security and
storage hand-off). Phase 4 makes everything operationally meaningful.

## Notes for AI Assistants Picking This Up

1. **Read this whole document first.** The hard constraints are not
   negotiable.
2. **Read `kubeswift_context.md`** for current project state.
3. **Read `docs/design/snapshots.md`** — this design assumes that
   exists and reuses its primitives.
4. **Phase 0 is mandatory.** Live migration has subtle failure modes;
   manual validation on the actual deployed CH version is required
   before writing controller code.
5. **VFIO is a hard constraint.** GPU and SR-IOV VMs do offline
   migration only. Document everywhere.
6. **CH version match is mandatory.** Across-version live migration
   does not work. Document the upgrade workflow.
7. **Don't promise sub-second downtime universally.** With shared
   storage on a fast network, sub-second is realistic for typical VMs.
   With RWO storage, downtime includes detach+attach time and is
   typically tens of seconds. With memory-pressure workloads, pre-copy
   may not converge. Set expectations honestly.
8. **mTLS is non-negotiable in production.** Cloud Hypervisor's
   plaintext TCP migration is a security concern. The mTLS sidecar
   pattern adds overhead but is the only safe default. An off-switch
   for fully-trusted clusters is acceptable; the default must be on.
9. **Drain integration uses the eviction API webhook.**
   Pod-deletion-prevention via webhook denial is the standard
   Kubernetes pattern; do not invent a new mechanism.
10. **The state machine is the controller's responsibility.**
    swiftletd reports facts via annotations; the controller decides
    phase transitions. Do not let swiftletd advance migration phase
    directly.
11. **Storage hand-off is per-disk.** Mixed access modes work fine if
    each disk's hand-off is handled correctly. Don't assume all disks
    have the same access mode.
12. **Network attachment compatibility is per-interface.** Multi-NIC
    VMs require all interfaces to be cross-node migratable, not just
    the primary.
13. **Helm chart sync.** After CRD changes:
    `make generate && cp config/crd/bases/*.yaml charts/kubeswift/crds/`.
14. **This is the largest design.** Resist the urge to combine phases
    or skip Phase 0. The complexity is real.
