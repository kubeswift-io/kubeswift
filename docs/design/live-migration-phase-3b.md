# Live Migration Phase 3b — Design Doc

> Phase 3b lights up `mode: live` on the migration foundation Phase 3a
> shipped (offline mode, SwiftMigration CRD, controller state machine,
> swiftletd action surface).
>
> Status: design lock-in. Implementation begins after this doc merges.
> Spike findings:
> [`live-migration-phase-3b-spike.md`](live-migration-phase-3b-spike.md).
> Phase 3a design (offline mode):
> [`live-migration-phase-3a.md`](live-migration-phase-3a.md).
>
> Last updated: 2026-05-15.

---

## 1. Overview

Phase 3b adds `mode: live` to the SwiftMigration controller. The
controller, swiftletd, and Cloud Hypervisor cooperate to transfer
guest memory + device state from a source launcher pod to a freshly-
launched destination launcher pod via Cloud Hypervisor's
`vm.send-migration` / `vm.receive-migration` RPCs over the default
pod network, then atomically cut over to the destination by deleting
the source pod after both swiftletds report the transfer complete.
Pre-copy iterations transfer the majority of guest memory while the
source vCPU keeps running; a brief stop-and-copy sub-phase inside
the destination CH instance transfers the remaining dirty pages
while vCPU is paused; the destination CH resumes the guest from
identical state.

**Operator-visible downtime is sub-3s** on the spike cluster for
4Gi-RAM RWX+Block disk-boot guests on `longhorn-migratable` storage.
This is the `observedDowntime` window from cutover-step-2 dispatch
on the source side to GuestRunning=True on the destination side. It
is the metric operators read in `kubectl describe smig` and the
metric capacity planning targets.

**Transfer duration is workload-sensitive.** Pre-copy iterations
plus the final stop-and-copy run for the wall-clock time it takes
Cloud Hypervisor to drive 5 pre-copy iterations + final
stop-and-copy at the pod-network bandwidth ceiling. The baseline
on a no-stress 4Gi guest is ~38s; under hostile memory-dirtying
workloads (50% of RAM continuously dirtied) it scales to ~87s.
Throughout this window the guest stays responsive — vCPU pauses
only for the final stop-and-copy sub-phase, which is sub-second on
the tested workloads.

**Workload classes shipped in Phase 3b** are the same as Phase 3a:

- Kernel-boot guests (`spec.kernelRef`), via SwiftKernel images.
- Disk-boot guests (`spec.imageRef`), with RWX+Block storage on a
  Longhorn `parameters.migratable: "true"` StorageClass.

VFIO/SR-IOV passthrough remains **rejected** for cross-node
migration — Phase 4+ work; the upstream Cloud Hypervisor constraint
that the runtime cannot release a VFIO device mid-migration
(<https://github.com/cloud-hypervisor/cloud-hypervisor/issues/2251>)
is unchanged. The webhook continues to reject `live` mode for
guests with VFIO/SR-IOV devices.

**Headline numbers** from spike Q2 (4Gi disk-boot guest, RWX+Block,
default pod network, `cloud-hypervisor v51.1`):

| Workload | `observedTransferDuration` | `observedDowntime` |
|---|---|---|
| Baseline (no stress) | 38.20s | 2.14s |
| LOW (1×64M stress-ng `rand-set`) | 45.04s | 2.04s |
| MED (2×256M `rand-set`) | 68.36s | 2.62s |
| HIGH (4×512M `rand-set` = 50% RAM) | 87.46s | 2.56s |

`observedDowntime` stays bounded ~2-3s across the entire tested
workload range. `observedTransferDuration` scales with dirty rate.
The decoupling is the load-bearing operator-UX property:
operators care about downtime; transfer duration is a planning
input, not an SLO.

---

## 2. Settled by spike

The Phase 3b spike resolved four architectural questions Phase 2
left open. Each settled question becomes a load-bearing constraint
for Phase 3b implementation — see also Section 9 for the formal
load-bearing property catalog.

### 2.1 Annotation control surface at state-machine granularity

**Spike Q1 — PASS conditional.** The swiftletd action/status
annotation surface (precedent: snapshot Phase 2 +
SwiftMigration Phase 3a) is the Phase 3b control plane. 4-6
annotation patches per migration; state-machine transitions
only.

Per-iteration progress reporting is **rejected by Phase 3b
design, independent of whether Cloud Hypervisor ever exposes
iteration boundaries**. Justification:

- Annotation round-trip latency is apiserver-bounded (~540ms
  median, ~740ms p95 measured in spike Q1). Per-iteration
  reporting at 50 iterations × 540ms = ~27s of pure overhead
  would dominate against the ~38s data-transfer body.
- Annotations are intentionally low-frequency state-machine
  transitions. Streaming data has different latency,
  frequency, and back-pressure characteristics; layering it
  on top of annotations corrupts both surfaces' semantic
  clarity.
- Future operator demand for in-flight progress visibility
  routes through a **separate streaming channel** (swiftletd
  HTTP status endpoint, upstream CH telemetry, or external
  observer pod), not through annotation extension.

Phase 3b ships a **best-effort heuristic progress estimate**
in the form `kubeswift.io/migration-progress-estimate: <N>`
(percent, monotonically increasing, capped at 95) emitted by
swiftletd-source at ~5s intervals during transfer. Section 5
covers the emission rate and computation; Section 9 LBA-5
codifies the surface scoping.

### 2.2 Pre-copy termination via CH iteration cap

**Spike Q2 — PASS through 50%-of-RAM dirtied.** Cloud
Hypervisor v51.1 hardcodes pre-copy to 5 iterations and
then **unconditionally** enters final stop-and-copy
regardless of remaining dirty pages. This is
**termination-by-iteration-cap, not classical algorithmic
convergence**.

The distinction matters for Phase 3b's webhook policy and
operator expectations:

- **Phase 3b webhook does NOT gate on dirty-rate estimation.**
  Hostile workloads complete because CH stops iterating, not
  because the algorithm decided "good enough." Admission
  rejection on the dirty-rate axis is the wrong policy
  surface for CH v51.x.
- **`spec.timeout` (existing Phase 3a, 5-minute default) is
  the runaway gate.** It covers the cluster-bandwidth-times-
  iteration-count worst case the spike measured (~105s
  total wall-clock for 50%-RAM-dirtied 4Gi).
- **The 5-iteration cap is a CH-version dependency.** A
  future CH version that makes the cap configurable, raises
  it, or replaces the unconditional cap with classical
  convergence detection would change Phase 3b's
  behavior:
  - **Configurable cap**: Phase 3b might want to expose it via
    `spec.maxIterations` (deferred; not in initial Phase 3b
    API).
  - **Classical convergence**: high-dirty-rate workloads
    could fail-by-timeout instead of always completing.
    Phase 3b webhook policy would need re-evaluation.
  - Track CH upstream behavior and re-run Q2-equivalent
    validation on any CH version upgrade that touches the
    pre-copy algorithm. Section 12 lists this as a future-
    phase prerequisite.

### 2.3 `newDstPod` clone-src as version-discipline mechanism

**Spike Q3 architectural finding.** Phase 3a's
[`internal/controller/swiftmigration/dst_pod.go::newDstPod`](../../internal/controller/swiftmigration/dst_pod.go)
constructs the destination pod by `srcPod.DeepCopy()` —
cloning the source pod's spec including launcher container
image — then resetting metadata and overriding only identity,
ownership, node pinning, and receiver-mode env. **This
structurally guarantees match-tag** at pod construction. No
controller code path produces a heterogeneous src/dst pair.

Consequences for Phase 3b:

- **The Phase 2 spike's proposed Decision 3 webhook rule is
  retired.** A webhook match-tag check would be redundant
  defense-in-depth; the implementation already enforces it
  atomically.
- **`spec.allowVersionSkew` is dropped from the Phase 3b API
  surface** (Section 3). The Phase 3a controller never
  consumed it; the field was inert.
- **A defensive image-tag-match check IS added at the
  Validating phase**, not as enforcement but as a fail-loud
  trip-wire: if a future refactor regresses `newDstPod`'s
  clone-src behavior, the controller's Validating phase
  surfaces the mismatch immediately rather than letting a
  heterogeneous migration silently proceed. See Section 4.

Section 9 LBA-1 documents the clone-src property formally and
flags the W26-pattern refactor risk: a future "let's re-resolve
dst pod from SwiftGuest spec — cleaner" refactor would silently
re-introduce the version-skew surface.

### 2.4 Default pod network as migration TCP channel

**Spike Q4 — PASS.** Default Calico VXLAN pod network at MTU
1450 saturates the underlying NIC (~902 Mbit/s on the
spike cluster's Hetzner gigabit interconnect) with low
retransmissions and no MTU sensitivity. CH live-migration
achieves **~95% of raw TCP bandwidth** (107.2 MB/s in CH
RPC ÷ 112.75 MB/s raw TCP). Orchestration overhead is ~5%.

Phase 3b uses the **default pod network for the migration TCP
channel**. No Multus, no SR-IOV, no dedicated migration
network. Operators on other CNI implementations may see
different efficiency floors; the empirical Calico VXLAN
floor is Phase 3b's documented baseline.

Operator sizing formula:

```
expected_transfer_duration ≈ (guest_RAM × 1.05) / pod_network_bandwidth
```

Section 6 surfaces this formula in operator documentation.

---

## 3. Operator-visible API surface

Phase 3b extends the SwiftMigration CRD shipped by Phase 3a.
Existing fields keep their semantics; new fields and rename are
listed below.

### 3.1 `spec.preferredMode`

Existing field (`migration.kubeswift.io/v1alpha1.SwiftMigrationSpec`).
Phase 3a shipped with three values; Phase 3b lights up `live`:

| Value | Phase 3a | Phase 3b |
|---|---|---|
| `auto` | Resolves to `offline` | Resolves per eligibility (Section 3.2) |
| `offline` | Active | Active (unchanged) |
| `live` | Webhook-rejected | Active (this design doc) |

Phase 3a operators submitting `live` got an admission rejection;
Phase 3b operators get the live path. The CRD field itself does
not change shape.

### 3.2 Auto-selection logic at Validating phase

When `spec.preferredMode == "auto"` (the default for
`swiftctl migrate <guest> --to <node>` without an explicit flag),
the Validating phase resolves the actual mode by computing
**live eligibility** from the guest's spec:

```
eligible_for_live = (
    guest.storage.access_mode == "ReadWriteMany"
    AND guest.storage.volume_mode == "Block"
    AND storage_class IS "longhorn-migratable"-class
    AND guest.has_vfio_devices == false
    AND guest.has_sriov_devices == false
)
```

Resolution:

- `eligible_for_live == true` → `status.mode = "live"`.
- `eligible_for_live == false` → `status.mode = "offline"`.

The Validating phase writes `status.mode` once and downstream
phases dispatch from `status.mode`, not from `spec.preferredMode`.
Operators read `status.mode` to confirm which path the controller
selected.

Auto-selection is a one-way function: it never escalates
mid-migration (operator cannot upgrade an offline-resolved
migration to live without creating a new SwiftMigration). This
keeps the state machine simple and matches Phase 3a's
write-once status semantics.

The storage-class match is a **class match, not name match**:
the Validating phase reads the SwiftGuest's bound PVC, looks up
the StorageClass, and checks
`parameters.migratable == "true"` (Longhorn-specific). Other CSI
drivers will surface their own equivalent attribute over time;
Phase 3b is Longhorn-only as a deliberate scope choice. Section
10 lists CSI driver matrix as out of scope.

### 3.3 Explicit `preferredMode: live` when ineligible

When operator submits `spec.preferredMode == "live"` and
eligibility computes `false`, two enforcement layers fail it:

**Webhook (`ValidateCreate`):** the SwiftMigration validating
webhook re-uses the eligibility computation from Section 3.2 and
rejects at admission with a structured error:

```
SwiftMigration "<name>" rejected:
  spec.preferredMode=live requires the source SwiftGuest to be live-
  migration-eligible. Current ineligibility reason: <reason>.
  Options: (a) change preferredMode to auto or offline; (b) reconfigure
  the SwiftGuest to live-eligible storage; (c) remove VFIO/SR-IOV
  devices.
```

**Controller (`Validating` phase):** if the SwiftMigration somehow
reaches the controller (admission bypass, or cluster-state changed
between admission and reconcile), the Validating phase re-checks
eligibility and transitions to `Failed` with a structured failure
reason (Section 4.7).

The webhook covers the operator's typo case; the controller
covers the cluster-state-drift case (e.g., the SwiftGuest's
PVC was rebound to a non-migratable StorageClass between
SwiftMigration admission and reconcile). This is the same
defense-in-depth shape as Phase 3a PR #26's per-operation
validation discipline.

### 3.4 `spec.allowVersionSkew` removed

Phase 3a CRD shipped `spec.allowVersionSkew bool` as a placeholder
for Phase 2's proposed match-tag policy escape hatch. Phase 3a's
controller never consumed it (because `newDstPod` clones src
image atomically; see Section 2.3). Phase 3b **removes the field
from the CRD**.

This is a **CRD breaking change** in the strict OpenAPI sense,
but no deployed manifests reference it (the field was inert from
day one). The CRD generator removes the field; the controller
deployment continues to accept Phase 3a-era SwiftMigration CRs
because Kubernetes silently drops unknown fields.

Migration discipline:

- Phase 3b release notes flag the removal explicitly.
- The Phase 3a → Phase 3b deployment runbook includes a step
  to re-apply CRDs (`make deploy` or `helm upgrade` covers
  this; manual `kubectl apply -k config/crd/bases/` operators
  must update their pipelines).
- No operator action needed for in-flight SwiftMigrations: the
  field has no behavioral effect.

### 3.5 `status.observedTransferDuration` (new, replaces `observedPauseWindow`)

Phase 3a shipped `status.observedPauseWindow`. The field
documentation was clarified in W27 commit D to state that it
measures the full `vm.send-migration` RPC duration (pre-copy
iterations + final stop-and-copy + finalize), most of which is
NOT vCPU-paused. The name is misleading; the Phase 3b spike Q2
results made the confusion concrete:

> A baseline migration shows `observedPauseWindow=38.20s` while
> the guest stayed responsive throughout. No operator looking at
> "pauseWindow=38s" intuits "guest was responsive for most of
> those 38s; only the last sub-second was actual vCPU-pause."

Phase 3b renames the field. The new name describes what the
field actually measures:

```yaml
status:
  observedTransferDuration: 38.217s      # NEW canonical name
  observedPauseWindow: 38.217s           # deprecated alias, populated for one release
```

**Deprecation cycle:**

- **Phase 3b release**: both fields populated. The swiftletd
  annotation surface and controller stamping logic are
  identical to Phase 3a but write **both** field names from a
  single source value (a tracked follow-up #11 in
  `kubeswift_context.md`). The new name is canonical; the old
  name is a deprecated alias.
  - Printer columns (`kubectl get smig -o wide`) display the
    new name.
  - `swiftctl migration describe` uses the new name.
  - CRD docstring on the old field reads `Deprecated:
    use observedTransferDuration. Will be removed in
    Phase 3b+1.`
- **Phase 3b+1 release**: `observedPauseWindow` is removed
  from the CRD. Operator tooling has one full release cycle
  to migrate.

The new field's CRD docstring explicitly references spike
Q2's empirical baseline (~38s for 4Gi guest with no stress)
and the formula in Section 2.4.

The `status.observedDowntime` field (anchored on
`status.cutoverStep2DispatchedAt` per W27a) is NOT renamed —
"downtime" describes what the field measures (operator-visible
cluster downtime window), and the spike Q2 results don't
expose any naming confusion.

### 3.6 Field summary

CRD changes (Go types + generated CRD YAML):

```diff
 type SwiftMigrationSpec struct {
     // ... existing fields ...
-    // AllowVersionSkew opts in to migration across heterogeneous CH versions.
-    // Phase 3a inert; Phase 3b removes.
-    AllowVersionSkew bool `json:"allowVersionSkew,omitempty"`
 }

 type SwiftMigrationStatus struct {
     // ... existing fields ...
+    // ObservedTransferDuration is the full vm.send-migration RPC duration
+    // (pre-copy iterations + final stop-and-copy + finalize). Replaces
+    // observedPauseWindow (deprecated alias).
+    ObservedTransferDuration *metav1.Duration `json:"observedTransferDuration,omitempty"`

-    // ObservedPauseWindow is the full vm.send-migration RPC duration.
+    // ObservedPauseWindow is a deprecated alias for ObservedTransferDuration.
+    // Will be removed in Phase 3b+1.
     ObservedPauseWindow *metav1.Duration `json:"observedPauseWindow,omitempty"`
 }
```

No changes to `SwiftGuestSpec`, `SwiftGuestClass`, or other
adjacent CRDs.

---

## 4. State machine for live mode

Phase 3a offline state machine:

```
Pending → Validating → Preparing → StopAndCopy → Resuming → Completed
                    ↘ Failed                              ↗
                      Cancelled
```

Phase 3b adds live-mode-specific phases that dispatch from
`status.mode == "live"`. The Validating phase is shared (it
writes `status.mode` then dispatches downstream); subsequent
phases branch on the resolved mode:

```
                                  ↗  Preparing → StopAndCopy ↘  (Phase 3a offline)
Pending → Validating  → mode=offline                          Resuming → Completed
                      ↘ mode=live                            ↗
                                  ↘  PreparingLive → StopAndCopyLive
                                       ↓                ↓
                                       Failed       Cancelled
```

The `Resuming` and `Completed` phases are shared between modes —
the destination guest healthy check and observedDowntime
stamping are mode-independent.

### 4.1 Validating phase (extended for live mode)

Validating phase adds the following checks **before** writing
`status.mode`:

| Check | Live mode | Offline mode |
|---|---|---|
| Source SwiftGuest exists | Yes | Yes (existing) |
| Source SwiftGuest is `Running` (not `Stopped`, `Failed`) | **Yes (new)** | Yes (existing) |
| Source pod is in `Running` phase | **Yes (new)** | No (Preparing handles stopped guests) |
| Target node `Ready` + has capacity | Yes | Yes (existing) |
| Storage is live-migration-capable (Section 3.2) | **Yes (new)** | No |
| No VFIO/SR-IOV devices | **Yes (new)** | No (offline tolerates) |
| Defensive image-tag-match check | **Yes (new)** | Not applicable |

**Defensive image-tag-match check**: re-runs `newDstPod`'s
implicit invariant — extracts the source pod's launcher image
tag, compares against the controller's default launcher image
tag. If they differ, the controller writes a `Compatible=False`
condition with reason `ImageTagMismatch` and transitions to
`Failed`. This check is **NOT** load-bearing enforcement (LBA-1
in Section 9 is); it is a trip-wire that fails loud if a future
`newDstPod` refactor regresses the clone-src guarantee. The
common path always passes because clone-src enforces the
invariant atomically.

After all checks pass:

1. Compute `eligible_for_live` per Section 3.2.
2. Resolve mode per Section 3.2 (auto) or Section 3.3 (explicit
   live).
3. Write `status.mode` (immutable thereafter for this CR).
4. Transition to `Preparing` (offline) or `PreparingLive` (live).

### 4.2 `PreparingLive` phase

The `PreparingLive` phase establishes the destination pod and
prepares it to receive the migration.

**Step 1 — annotation idempotency marker** (Phase 3a precedent):

Patch `kubeswift.io/migration-in-progress: <SwiftMigration-UID>`
on the source SwiftGuest's metadata. This is the
source-of-truth for re-entrant reconciles (leader handover,
controller restart): if the annotation is present and the
in-progress UID matches, the controller skips re-doing
Validating-side work and resumes from the current phase.

**Step 2 — destination pod creation**:

Call `newDstPod(mig, guest, srcPod, scheme)` (existing,
[`dst_pod.go::newDstPod`](../../internal/controller/swiftmigration/dst_pod.go)).
This:

- DeepCopies `srcPod.Spec` (image and all — see Section 2.3,
  Section 9 LBA-1).
- Resets metadata: drops UID, ResourceVersion, OwnerReferences,
  Finalizers.
- Sets pod identity: `<guestName>-mig-<short-UID>`, labels
  including `kubeswift.io/migration: <SwiftMigration-name>` and
  `kubeswift.io/migration-role: destination`.
- Sets pod ownership: SwiftGuest is the controller-owner.
- Overrides node pinning: `pod.Spec.NodeName = target.nodeName`.
- Adds receiver-mode env: `KUBESWIFT_MIGRATION_ROLE=receive`.

Submit pod via Create. Idempotency: if a pod with the expected
name already exists (leader-handover replay), compare
spec.image atomically and adopt; if image mismatches, transition
to `Failed` with reason `DstPodConflict`.

**Step 3 — wait for destination pod Running**:

Standard controller-runtime watch-driven wait. Phase 3a's pod-
observed watch handles both `migration-role: source` and
`migration-role: destination` (Phase 3a Group C label-based
watch addition).

**Step 4 — annotation handshake (receive-ready)**:

Patch `kubeswift.io/migration-action: receive` on the destination
pod (sequential handshake per D4a accepted (i)):

- The destination swiftletd's action handler picks up the
  receive action, transitions through internal sub-states
  (`receive-init` → `receive-listening` → `receive-ready`),
  and writes `kubeswift.io/migration-status: receive-ready`
  on the destination pod once the TCP listener is open and CH
  is ready to accept `vm.receive-migration`.
- The controller watches for the status annotation and
  transitions to `StopAndCopyLive` only when `receive-ready`
  is observed.

**Phase failure surface** during `PreparingLive`:

- Destination pod fails to schedule (target node disappeared,
  no capacity): controller deletes the dst pod attempt,
  transitions to `Failed` with reason `DstScheduleFailed`.
- Destination pod stuck `Pending` past timeout: same.
- Destination pod runs but never writes `receive-ready` past
  the destination-listener timeout (default ~60s): controller
  patches `cancel` on destination pod and transitions to
  `Failed` with reason `DstNeverReady`.
- Destination swiftletd reports `migration-status: failed`
  during PreparingLive (e.g., port-bind failure, CH spawn
  error): controller transitions to `Failed` with structured
  reason.

All `Failed` transitions in `PreparingLive` are **pre-cutover
failures**: the source guest is untouched and remains running.
The cleanup path deletes the destination pod (since it's now
orphaned) and restores the source SwiftGuest's
`migration-in-progress` annotation to empty.

### 4.3 `StopAndCopyLive` phase

The `StopAndCopyLive` phase drives the actual memory transfer.
The phase is a 6-substate state machine inside the controller,
each substate gated by an annotation observation from the
source or destination swiftletd. Substate names mirror Phase
3a's StopAndCopy substate naming convention:

```
substateInit → substateSrcSendDispatched → substateSrcSending
            → substateSrcCompleted → substateDstCompleted
            → substateCutoverDispatched → substateCutoverComplete
```

Walking through each substate:

**`substateInit`** (controller entry):
- Patch `kubeswift.io/migration-action: send` on the source pod.
- Pass the destination pod's IP via SwiftMigration CR
  (`status.targetIP`, populated at PreparingLive step 4 from
  the dst pod's status).
- Patch annotation `kubeswift.io/migration-target-ip: <ip>` on
  source pod (the swiftletd reads this to construct the
  vm.send-migration request).
- Transition to `substateSrcSendDispatched`.

**`substateSrcSendDispatched`**:
- Source swiftletd's action handler observes
  `migration-action: send`, validates the URL/ack annotations,
  writes `kubeswift.io/migration-status: sending`, and calls
  Cloud Hypervisor's `vm.send-migration` HTTP API.
- Controller observes `migration-status: sending` and
  transitions to `substateSrcSending`.

**`substateSrcSending`** (longest substate, ~38-87s on spike):
- CH drives 5 pre-copy iterations + final stop-and-copy +
  finalize internally. swiftletd's call to `vm.send-migration`
  is synchronous; the action handler thread blocks here.
- Source swiftletd writes
  `kubeswift.io/migration-progress-estimate: <N>` at ~5s
  intervals (Section 5.4 covers computation).
- Controller watches but does not gate on progress
  annotations — they are informational only.
- When CH's `vm.send-migration` returns successfully,
  source swiftletd writes:
  - `kubeswift.io/migration-status: send-complete`
  - `kubeswift.io/migration-transfer-duration-ms: <ms>`
    (full RPC wall-clock elapsed)
- Controller observes `send-complete` and transitions to
  `substateSrcCompleted`.

**`substateSrcCompleted`**:
- Controller reads
  `kubeswift.io/migration-transfer-duration-ms` from the
  source pod and stamps
  `status.observedTransferDuration` (and the deprecated alias
  `status.observedPauseWindow`).
- Controller waits for `kubeswift.io/migration-status:
  receive-complete` from the destination pod. This is the W1
  gate observation pattern from Phase 3a: the destination
  swiftletd writes `receive-complete` once
  `vm.receive-migration` returns and CH is ready to resume.
- Controller transitions to `substateDstCompleted` on
  observation.

**`substateDstCompleted`**:
- Both swiftletds have reported success. The migration data
  path is complete; the destination CH is paused with the
  guest's memory + device state loaded, ready to resume.
- Controller dispatches cutover step 1 (existing Phase 3a
  cutover, [`cutover.go::cutoverStep1`](../../internal/controller/swiftmigration/cutover.go)):
  patch `status.podRef.Name` on the SwiftGuest to the
  destination pod's name. This is the K8s-observable cutover
  point — `kubectl get smig -w` and `swiftctl describe` flip
  immediately.
- Controller stamps `status.cutoverStep1At`.
- Controller dispatches cutover step 2:
  - Stamps `status.cutoverStep2DispatchedAt` (W27a anchor).
  - Calls Delete on the source pod (graceful, with a 30s
    deadline — same as Phase 3a `cutoverStep2`).
- Controller transitions to `substateCutoverDispatched`.

**`substateCutoverDispatched`**:
- Controller waits for source pod's deletion event (Pod
  NotFound from watch).
- On NotFound, controller patches
  `kubeswift.io/migration-action: resume` on destination pod.
  This is **new in Phase 3b** — Phase 3a's offline path uses
  the SwiftGuest controller's standard pod-creation flow to
  resume; Phase 3b's live path uses an explicit resume
  annotation because the destination CH is already running
  with the migrated state and just needs to leave the post-
  receive paused state.
- Destination swiftletd observes `resume` action, calls
  Cloud Hypervisor's `vm.resume` HTTP API, writes
  `kubeswift.io/migration-status: resumed`.
- Controller observes `resumed`, transitions to
  `substateCutoverComplete`.

**`substateCutoverComplete`**:
- Controller transitions phase: `StopAndCopyLive` → `Resuming`.

### 4.4 `Resuming` phase

The `Resuming` phase is shared between modes (existing Phase
3a logic at
[`resuming_live.go`](../../internal/controller/swiftmigration/resuming_live.go)
+ resuming.go).

For live mode:

1. Wait for destination guest's `GuestRunning=True` condition
   (swiftletd-reported via post-resume health probe).
2. Stamp `status.observedDowntime` =
   `GuestRunningObservedAt - cutoverStep2DispatchedAt` (W27a).
3. Stamp `status.completedAt = metav1.Now()`.
4. Transition to `Completed`.

### 4.5 `Completed` phase

Terminal state. Controller stops reconciling per Phase 3a
PR #25/#26 terminal-state discipline. Phase 3b adds nothing
here.

### 4.6 Cancel handling

Cancel is operator-initiated: `spec.cancelRequested = true`.
The cancel handler (Phase 3a, [`cancel_live.go`](../../internal/controller/swiftmigration/cancel_live.go))
extends for live mode per D4b accepted (ii) — annotation
broadcast to both pods:

- Controller patches `kubeswift.io/migration-action: cancel`
  on **both** source and destination pods.
- Source swiftletd observes cancel:
  - If still in `substateSrcSending`: calls CH's
    `vm.cancel-migration` HTTP API. CH returns from
    `vm.send-migration` with a `MigrationCanceled` error.
  - Writes `kubeswift.io/migration-status: cancelled`.
- Destination swiftletd observes cancel:
  - Closes the TCP listener (if pre-active).
  - If in receive-active: CH's `vm.receive-migration` returns
    with `MigrationCanceled` once src cancels.
  - Releases the per-pod PVC attachment (best-effort).
  - Writes `kubeswift.io/migration-status: cancelled`.
- Controller observes both cancellations, transitions to
  `Cancelled`.
- Cleanup: deletes the destination pod (source guest stays
  on src node, untouched).

**Cancel timing semantics**:

- Cancel pre-cutover (during PreparingLive or substateInit ..
  substateSrcSending): source guest fully recovers. No data
  loss. `Cancelled` is the terminal state.
- Cancel post-cutover (substateDstCompleted onwards): **cancel
  is IGNORED**. The CancelIgnored gate (Phase 3a
  [`stopandcopy_live.go`](../../internal/controller/swiftmigration/stopandcopy_live.go)
  W21 fix, `SwiftMigrationConditionPodRefSwapped` gate) covers
  the narrow Resuming window where cancelling could destroy
  the migrated guest.
- Phase 3b inherits this gate without modification.

### 4.7 Failure reason taxonomy

When the migration fails mid-flight, the controller transitions
to `Failed` and writes structured fields per D4c accepted (ii):

```yaml
status:
  phase: Failed
  failureReason: <enum>            # canonical category
  failureReasonCode: <enum>        # narrow code for tooling
  failureMessage: <human-readable> # operator-visible diagnostic
```

**`failureReasonCode` enum (Phase 3b additions over Phase 3a)**:

| Code | Origin | Meaning |
|---|---|---|
| `Cancelled` | Phase 3a | Operator requested cancel; took effect pre-cutover. |
| `PodTerminated` | Phase 3a | Source or destination pod was K8s-deleted mid-flight. |
| `SourcePodReplaced` | Phase 3a (W26) | Source pod UID drifted; chain-migration race. |
| `Timeout` | Phase 3a | `spec.timeout` exceeded. |
| `Other` | Phase 3a | Unclassified. |
| `EligibilityMismatch` | **3b new** | Explicit `live` with ineligible storage / VFIO. |
| `DstScheduleFailed` | **3b new** | Destination pod could not schedule. |
| `DstNeverReady` | **3b new** | Destination listener didn't reach `receive-ready` in time. |
| `ReceiveDisconnect` | **3b new** | CH `vm.receive-migration` saw the source disconnect. |
| `RpcError` | **3b new** | CH `vm.send-migration` returned a non-cancel error. |
| `ImageTagMismatch` | **3b new** | Defensive trip-wire (Section 4.1); should never fire. |
| `DstPodConflict` | **3b new** | Idempotency check found a dst pod with conflicting image. |

The `failureReason` field (broader category) groups the codes
above for `kubectl describe smig` summarization. The
`failureReasonCode` field is what swiftctl / operator tooling
parses.

The swiftletd annotation surface carries the structured code
via `kubeswift.io/migration-failure-reason-code: <enum>` (the
controller maps annotation values to the CRD enum). The
mapping table is implemented in Phase 3b PR 2.

---

## 5. swiftletd action surface

Phase 3a action surface (existing):

- `migration-receive` action — destination pod-mode.
- `migration-send` action — source pod-mode.
- `migration-cancel` action — both pod-modes.

Phase 3b extends these with sub-state machines and additional
status annotations. The annotation key schema and
ack-gate-required-for-each-write semantics from Phase 3a
PR-B are unchanged.

### 5.1 `migration-receive` action — receive state machine

The destination swiftletd's receive action runs an internal
state machine. **States are swiftletd-internal**; controller-
visible signals are the published status annotations.

```
receive-init → receive-listening → receive-ready
            → receive-active → receive-complete | receive-failed
```

| Internal state | Trigger / observation | Controller observes |
|---|---|---|
| `receive-init` | Action handler picks up `migration-action: receive`. Parses inputs (target-IP, port, runtime dir). | — |
| `receive-listening` | swiftletd calls CH's `vm.create` (paused) + opens TCP listener on the migration port. | — |
| `receive-ready` | TCP listener confirmed bound; CH ready to accept `vm.receive-migration`. swiftletd writes `kubeswift.io/migration-status: receive-ready`. | **`receive-ready`** → gate for substateInit (Section 4.3). |
| `receive-active` | swiftletd's blocking call to CH's `vm.receive-migration` is in flight; CH is consuming the TCP stream. swiftletd may write `kubeswift.io/migration-status: receive-active` (informational, optional). | `receive-active` (informational). |
| `receive-complete` | CH's `vm.receive-migration` returned successfully; guest paused on dst, ready to resume. swiftletd writes `kubeswift.io/migration-status: receive-complete`. | **`receive-complete`** → gate for substateDstCompleted. |
| `receive-failed` | CH returned an error, listener disconnected, or cancel fired. swiftletd writes `kubeswift.io/migration-status: failed` + `kubeswift.io/migration-failure-reason-code: <code>`. | `failed` + code → controller transitions to Failed. |

Listener timeout: swiftletd-internal default 60s for the
`receive-listening` → CH `vm.receive-migration` accept
window. If the source never connects, swiftletd writes
`receive-failed` with code `DstNeverReady`-equivalent. The
controller's `PreparingLive` step 4 timeout (Section 4.2) is
the broader cluster-level gate; swiftletd's listener timeout
is the inner gate.

### 5.2 `migration-send` action — send state machine

The source swiftletd's send action is simpler — Cloud Hypervisor's
`vm.send-migration` is synchronous and either completes or
errors; there is no observable internal state during the call.

```
send-init → send-active → send-complete | send-failed
```

| Internal state | Trigger / observation | Controller observes |
|---|---|---|
| `send-init` | Action handler picks up `migration-action: send`. Validates ack annotation (Phase 2 S2 gate) and target-IP. | — |
| `send-active` | swiftletd writes `kubeswift.io/migration-status: sending`. Calls CH `vm.send-migration` (synchronous, blocking). During this state, swiftletd emits progress estimate (Section 5.4). | `sending` (gate for substateSrcSending). Progress estimate during. |
| `send-complete` | CH returns successfully. swiftletd writes `migration-status: send-complete` + `migration-transfer-duration-ms: <ms>`. | **`send-complete`** + duration → gate for substateSrcCompleted. |
| `send-failed` | CH returns an error (RpcError) or peer disconnect detected (ReceiveDisconnect). swiftletd writes `failed` + structured code. | `failed` + code → controller transitions to Failed. |

The Phase 2 PR-B sanitizer (CH error string → category token)
maps CH's raw error strings to the Phase 3b reason code enum
(Section 4.7).

### 5.3 `migration-cancel` action — receiver-side semantics

The cancel action's source-side semantics are inherited from
Phase 2 PR-B: swiftletd-source observes
`migration-action: cancel`, calls CH's `vm.cancel-migration`
HTTP API, and writes `migration-status: cancelled`. Phase 3b
implements the **receiver-side** cancel semantics that Phase
2 left as a TODO:

- swiftletd-destination observes `migration-action: cancel`:
  - If `receive-listening`: close TCP listener, write
    `migration-status: cancelled`, exit action handler.
  - If `receive-active`: CH's `vm.receive-migration` returns
    with `MigrationCanceled` once the source's `vm.cancel-
    migration` triggers a disconnect on the wire. swiftletd
    writes `cancelled` after the CH call returns.
  - If `receive-complete` (post-cutover): cancel is **ignored**
    (matches controller-side CancelIgnored gate, Section 4.6).

### 5.4 Send progress estimate

swiftletd-source emits a best-effort heuristic progress
annotation during `send-active`:

```
kubeswift.io/migration-progress-estimate: <integer 0-95>
```

**Emission rate**: every ~5s during the `vm.send-migration`
call (~5-8 patches per migration on the spike workload range).
Within Q1's annotation latency budget (Section 2.1).

**Computation**:

```rust
let elapsed_s = (now - send_started_at).as_secs() as f64;
let guest_ram_mb = intent.memory as f64;
let pod_network_baseline_mbps = 108.0;  // spike Q2 inferred CH throughput
let expected_s = guest_ram_mb / pod_network_baseline_mbps;
let raw_estimate = 100.0 * elapsed_s / expected_s;
let capped = raw_estimate.min(95.0).max(0.0) as i64;
patch_annotation("kubeswift.io/migration-progress-estimate", capped);
```

**Honest framing constraints**:

- The `-estimate` suffix in the annotation key name makes
  the heuristic nature explicit. swiftctl + kubectl output
  surfaces the annotation with a parenthetical "(estimate)"
  qualifier (Section 6).
- Cap at 95%: prevents operators seeing "100% complete"
  while the RPC is still in finalize. The transition from
  95% to send-complete observation is the next discrete
  event.
- Floor at 0: defensive against clock drift on swiftletd
  startup.
- Baseline 108 MB/s is **Calico-VXLAN-specific** (spike Q4).
  Operators on other CNIs see different baselines; the
  estimate is least accurate on cluster configurations
  furthest from spike. Documented in Section 10 caveats.
- Computation runs inline in the action handler, not on a
  separate timer thread (simpler; ~5s granularity is fine).

**No status field**: progress estimate is annotation-only,
NOT mirrored to a CRD status field. The transient nature of
progress data (changes every 5s, valid for ~30-100s)
doesn't match status semantics; persisting it across
post-completion reconciles would be misleading. swiftctl
reads the annotation directly when describing a migration
in `StopAndCopyLive` phase.

### 5.5 Annotation key catalog (Phase 3b)

Consolidated reference for the Phase 3b annotation surface
(all under the `kubeswift.io/` prefix):

| Key | Pod | Writer | Direction | Purpose |
|---|---|---|---|---|
| `migration-action: receive` | dst | controller | C→S | Action dispatch. |
| `migration-action: send` | src | controller | C→S | Action dispatch. |
| `migration-action: cancel` | both | controller | C→S | Cancel broadcast. |
| `migration-action: resume` | dst | controller | C→S | Post-cutover resume trigger (new in 3b). |
| `migration-target-ip: <ip>` | src | controller | C→S | Destination IP for `vm.send-migration`. |
| `migration-phase2-unsafe-plaintext: ack` | both | controller | C→S | Phase 2 S2 ack gate (unchanged). |
| `migration-status: <substate>` | both | swiftletd | S→C | State machine published transitions. |
| `migration-progress-estimate: <0..95>` | src | swiftletd | S→C | Heuristic progress (Section 5.4). |
| `migration-transfer-duration-ms: <ms>` | src | swiftletd | S→C | RPC wall-clock for `observedTransferDuration` stamping. |
| `migration-failure-reason-code: <enum>` | both | swiftletd | S→C | Structured failure code. |
| `migration-in-progress: <UID>` | SwiftGuest | controller | C→C | Idempotency marker (Phase 3a). |

`C→S` = controller → swiftletd; `S→C` = swiftletd → controller;
`C→C` = controller-internal idempotency.

Total patches per successful migration: ~6-8 controller-side
writes (action transitions) + ~5-8 swiftletd-side writes
(status + progress) + 1 idempotency = ~12-17 annotation patches
per migration. Well within Q1's latency budget.

---

## 6. Operator UX

Phase 3b extends the operator-facing surface (swiftctl,
`kubectl describe smig` output, operator runbook) without
changing the shape of Phase 3a's UX.

### 6.1 `swiftctl migrate`

Existing Phase 3a flag set:

```
swiftctl migrate <guest> --to <node> [--reason "..."] [--timeout 10m]
```

Phase 3b adds `--preferred-mode`:

```
swiftctl migrate <guest> --to <node> --preferred-mode {auto,live,offline}
```

- Default: `auto` (preserves Phase 3a default behavior).
- `live`: explicit live request. Controller / webhook fail it
  if the guest is ineligible (Section 3.3).
- `offline`: explicit offline request. Always succeeds (offline
  works for all live-eligible guests too — offline is a
  superset of live's eligibility set).

The flag maps directly to `spec.preferredMode` on the
generated SwiftMigration CR.

### 6.2 `swiftctl migration describe`

Phase 3a output during the offline `StopAndCopy` phase:

```
Phase: StopAndCopy
Phase detail: transferring guest state
```

Phase 3b output during the live `StopAndCopyLive` phase:

```
Phase: StopAndCopyLive
Phase detail: transferring guest state (substateSrcSending)
Progress (estimate): 47%
  Note: Progress is a heuristic based on baseline ~108 MB/s pod
  network bandwidth (spike Q4). Actual rate depends on workload
  memory dirty rate.
```

The "Note:" wrapper makes the heuristic nature unmissable for
operators who haven't internalized Section 5.4's caveats.

Phase 3b output after `Completed`:

```
Phase: Completed
Downtime: 2.14s             # operator-visible cutover window (W27a)
Transfer duration: 38.22s   # full vm.send-migration RPC (renamed from pauseWindow)
Mode: live                  # resolved from auto if auto was requested
```

The two metrics carry different meanings — the describe output
adds a one-line semantic gloss:

```
Downtime is the operator-visible cluster downtime window
(cutover step 2 dispatch → guest healthy on destination).
Transfer duration is the full vm.send-migration RPC duration
(pre-copy iterations + final stop-and-copy + finalize);
guest stays responsive throughout most of this window.
```

### 6.3 `kubectl describe smig` (controller-rendered)

`kubectl describe` reads CRD printer columns + status fields.
Phase 3b PR 3 updates the SwiftMigration CRD printer columns
to surface the new field names:

```diff
- additionalPrinterColumns:
-   - name: Phase
-   - name: Mode
-   - name: Downtime
-   - name: PauseWindow
+ additionalPrinterColumns:
+   - name: Phase
+   - name: Mode
+   - name: Downtime
+   - name: Transfer
```

`PauseWindow` column → `Transfer` column reading from
`observedTransferDuration`. The deprecated alias
`observedPauseWindow` is still populated but not surfaced in
the wide-output columns.

### 6.4 Operator runbook (`docs/migration/phase-3b.md`)

Phase 3b PR 3 ships an operator runbook at
`docs/migration/phase-3b.md`. Contents:

- **Quick start**: `swiftctl migrate <guest> --to <node>` with
  no flags resolves to live automatically for eligible guests.
- **Eligibility check**: how to confirm a SwiftGuest is live-
  eligible (`kubectl get sg <name> -o yaml | grep storage`).
- **Reading migration metrics**: distinction between
  `observedDowntime` and `observedTransferDuration`. The
  semantic gloss from Section 6.2 expanded with two empirical
  examples (no-stress 4Gi and stress-ng MED 4Gi).
- **Caveats**:
  - Progress estimate is best-effort; can over/under-predict
    on hostile workloads.
  - IP changes across nodes on default networking (unchanged
    from Phase 3a; references Tracked Follow-up #1 for
    multi-node L2 work).
  - VFIO/SR-IOV cross-node migration not yet supported
    (unchanged from Phase 3a).
  - CPU-feature mismatch is not detected by the validation
    pipeline; operator runbook discipline (`lscpu`
    uniformity check before enabling live migration).
    Future Phase 3c may add `swiftctl migrate --check` (see
    Tracked Follow-up #10).
  - Cluster swiftletd rollout in progress: complete the
    rollout before triggering live migrations. The clone-src
    pod-builder behavior means a live migration uses the src
    pod's image; if the src pod is on an older swiftletd
    image but the dst node has only the new one cached, the
    pull may bog down. Same shape as v0.1.0's CH-upgrade
    discipline.
- **Troubleshooting**:
  - SwiftMigration stuck in `PreparingLive`: check destination
    pod's image-pull status, listener bind, ack annotation.
  - SwiftMigration `Failed` with `EligibilityMismatch`:
    operator changed storage class between guest creation and
    migration submission; resubmit after reconfirming
    eligibility.
  - SwiftMigration `Failed` with `RpcError`: CH-level
    error during transfer; tail the source pod's logs for
    the un-sanitized message.
- **Walkthrough log links**: per Phase 3a per-PR walkthrough
  discipline (Section 7).

---

## 7. Implementation gates

Per Phase 3b implementation PR (D7 accepted (iii) —
walkthrough-as-gate):

1. **Unit tests green** for new code paths. Phase 3a's test
   patterns hold (fake-client controller tests,
   selectiveFailingClient reconcile-recovery tests).
2. **Build clean** (Go + Rust): `make build` + `cargo build
   --release` succeed without warnings beyond the project's
   existing baseline.
3. **Smoke test passes** (regression-only for migration code;
   the smoke test exercises baseline boot scenarios that
   should remain unaffected by Phase 3b's additions).
4. **Cluster validation**: at least one successful live
   migration end-to-end on the deployed image. PR 1 manual
   demonstration; PR 2 end-to-end via SwiftMigration CR; PR
   3 swiftctl-flow validation.
5. **Walkthrough findings documented** per Phase 3a per-PR
   walkthrough discipline:
   - LOW findings: filed as tracked follow-ups in
     `kubeswift_context.md`.
   - MEDIUM findings: fixed before the next PR begins
     (in-flight or as a hotfix PR).
   - HIGH findings: block the next PR entirely; must be
     resolved before the PR can merge.
6. **Status field semantic audit** (D8c accepted): for each
   new status field added in the PR, audit the docstring
   against what the implementation writes on real cluster
   output. Document the audit in the walkthrough findings doc.
   Pattern surfaced by W27 + W27 commit D in Phase 3a — a
   field's docstring must match what the implementation
   actually computes, especially for fields like
   `observedTransferDuration` where naming carries semantic
   weight.

**Phase 3b walkthroughs should be tighter than Phase 3a's.**
The Phase 3b spike eliminated most of the architectural
surface area (annotation surface, convergence, version
discipline, network channel). Walkthroughs catch operational
gaps — RBAC missing for new watch resources, CRD field
silently stripped on stale-CRD clusters, controller-runtime
informer cache requirements (the W7/W8/W26 pattern).

---

## 8. PR split

Phase 3b ships in **three PRs** (D5 accepted (ii) — milestone-
based; manual-demo / controller / UX). Each PR has a manual
demonstration milestone and a walkthrough gate.

### 8.1 PR 1 — swiftletd + swift-ch-client + CRD rename

**Scope**:

- `swift-ch-client` gains `vm.send-migration` and
  `vm.receive-migration` HTTP API methods (Phase 2 PR-A
  shipped the primitives; Phase 3b PR 1 finalizes them with
  Phase 3b's error sanitization mapping).
- swiftletd gains the receive state machine (Section 5.1):
  `receive-init` → `receive-listening` → `receive-ready` →
  `receive-active` → `receive-complete | receive-failed`.
- swiftletd gains the send state machine (Section 5.2) with
  progress-estimate emission (Section 5.4).
- swiftletd action handler dispatches receive / send / cancel
  / resume actions with the new sub-states.
- CRD rename: add `status.observedTransferDuration`; keep
  `status.observedPauseWindow` as a deprecated alias (both
  populated by the controller from the same source value).
  Drop `spec.allowVersionSkew`.
- Controller side: status-stamping plumbing for the new field
  + deprecated alias. NO controller live-mode dispatch yet —
  PR 1 is annotation-driven only.

**Manual demonstration possible at end of PR 1**: operator
launches two pods manually (source on miles, destination on
boba), hand-triggers receive then send annotations, observes
live migration succeed via CH RPC traces and SwiftGuest
status flip. No SwiftMigration CR involvement.

**Walkthrough scope**:

- Manual demonstration on 4Gi disk-boot RWX+Block guest.
- Regression check against Phase 3a offline path (must still
  work; the CRD rename should not break offline migration's
  metric population).
- One offline migration round-trip + one manual live-migration
  round-trip.

**Estimated LOC**: ~10,000 LOC across Go (CRD types,
swift-ch-client client extensions, controller stamping logic
+ deprecation alias plumbing) and Rust (swiftletd receive
state machine, progress emitter, action dispatcher
extensions).

### 8.2 PR 2 — controller integration

**Scope**:

- Controller live-mode dispatch in `Validating`,
  `PreparingLive`, `StopAndCopyLive`, `Resuming` phases
  (Section 4).
- Auto-selection logic at `Validating` phase per Section 3.2.
- Eligibility check at controller-side `Validating` phase
  (defense in depth) per Section 3.3.
- Webhook validation for explicit-live-when-ineligible per
  Section 3.3.
- Cancel handshake (both-pod broadcast + CancelIgnored gate
  inheritance) per Section 4.6.
- Structured failure reason code stamping per Section 4.7.

**End-to-end at end of PR 2**: SwiftMigration with `auto`
preferredMode resolves to `live` for eligible guests, triggers
PR 1's swiftletd surface, succeeds. SwiftMigration with
explicit `live` for ineligible guests is rejected.

**Walkthrough scope**:

- Live migration end-to-end via SwiftMigration CR on three
  workload classes (Q2 spike LOW / MED / HIGH).
- Auto selection works correctly: kernel-boot guest resolves
  to live; offline-eligible-but-not-live guest resolves to
  offline.
- Explicit live request on VFIO guest is rejected by webhook;
  explicit live request that bypasses webhook (e.g.,
  controller-internal CR creation) is rejected at controller-
  Validating.
- Chain migration: miles→boba→miles round-trip succeeds
  (W26 regression check).
- Cancel pre-cutover: SwiftMigration `Cancelled`, source guest
  intact.
- Cancel post-cutover (within Resuming window): cancel
  ignored, migration completes (CancelIgnored gate
  verification).

**Estimated LOC**: ~7,000 LOC across controller (state machine
extension across PreparingLive / StopAndCopyLive / cutover
extension for `resume` action / failure-reason mapping) and
webhook (eligibility-check extension; explicit-live admission
rule).

### 8.3 PR 3 — swiftctl + observability

**Scope**:

- `swiftctl migrate --preferred-mode` flag (Section 6.1).
- `swiftctl migration describe` surfaces progress estimate
  during `StopAndCopyLive` phase + new metric names after
  `Completed` (Section 6.2).
- `kubectl describe smig` printer columns update (Section
  6.3).
- Phase 3b operator runbook at `docs/migration/phase-3b.md`
  (Section 6.4).
- W27-commit-D-shape docstring updates for the new status
  fields (CRD docstrings reflect actual semantics, NOT just
  field name implications).

**Walkthrough scope**:

- Operator UX flows: `swiftctl migrate`, `swiftctl migration
  describe`, `kubectl describe smig` all surface the new
  fields correctly.
- Progress estimate accuracy under realistic workloads
  (regression against Q2 spike baseline; not perfect — the
  heuristic is best-effort).
- Operator runbook covers known edge cases (rollout-in-
  progress, CPU-feature mismatch caveats, ineligibility
  troubleshooting).

**Estimated LOC**: ~3,000 LOC across Go (swiftctl flag
plumbing, kubectl describe printer columns wiring) and Markdown
(operator runbook).

### 8.4 Total Phase 3b footprint

Approximately **20,000 LOC** across three PRs. Comparable to
Phase 3a's PR 1 (Group B + Group C) but spread across more
discrete milestones because Phase 3b's surface is
implementation-light at the controller layer (the architectural
work is already done) and implementation-heavy at the swiftletd
layer (the receive state machine + progress emitter are new).

---

## 9. Load-bearing architectural properties

Phase 3b's design depends on five properties that are
**load-bearing** — a refactor that touches any of them, even
for cosmetic cleanup, can silently regress Phase 3b without
test surface to catch it. Document each property explicitly so
future maintainers see the constraint before touching the
relevant code. A refactor that changes one of these must either
explicitly preserve the property or update this design doc.

### LBA-1: `newDstPod` clone-src guarantees match-tag migration

**Where**:
[`internal/controller/swiftmigration/dst_pod.go::newDstPod`](../../internal/controller/swiftmigration/dst_pod.go),
specifically the `srcPod.DeepCopy()` call at line 158.

**What**: the destination pod inherits the source pod's spec
including launcher container image atomically. No controller
code path produces a heterogeneous src/dst pair.

**Why it's load-bearing**: Phase 2 spike's proposed webhook
version-discipline rule (Decision 3) was retired because this
clone-src behavior already enforces match-tag. Phase 3b ships
with NO webhook match-tag rule because the implementation is
the enforcement.

**Refactor risk**: a future change that re-resolves the
destination pod spec from `SwiftGuest.spec` instead of cloning
the source pod (which would sound cleaner architecturally —
"single source of truth, no clone-and-mutate") silently
re-introduces the version-skew surface. Same architectural
pattern as W26 (single-mode fix introduces other-mode
regression).

**Discovered by**: Phase 3b spike Q3 (architectural finding,
2026-05-08).

**Tracked in**: `kubeswift_context.md` Tracked Follow-up #12.
Cross-referenced from the `newDstPod` docstring (lines 108-130).

**Defensive trip-wire**: Section 4.1 image-tag-match check at
the Validating phase fires loud if this property is regressed.
Common path always passes; failure surface points operators at
this LBA section.

### LBA-2: `srcPodLookupName` invariant locks identity at Validating

**Where**:
[`internal/controller/swiftmigration/validating_live.go::srcPodLookupName`](../../internal/controller/swiftmigration/validating_live.go)
+ the three downstream call sites (`stopandcopy_live.go`,
`cutover.go`, `preparing_live.go`) that use it via the helper.

**What**: the source pod name is locked at Validating phase
via `status.SourcePodRef.Name = srcPod.Name` and looked up
consistently downstream via `srcPodLookupName(mig, guest)`.
Prevents the W15 → W26 single-mode-fix-broke-chain-migration
regression.

**Why it's load-bearing**: chain migrations rely on this
invariant. After a prior migration's cutover,
`SwiftGuest.status.podRef.Name` points at the prior dst pod
(= the new migration's src), not `guest.Name`. Literal
`guest.Name` lookup hits NotFound; naïve
`canonicalPodNameForGuest` resolves to the new migration's
own dst pod after cutoverStep1 (silent data destruction at
cutoverStep2 — the W26 finding from E12 2026-05-04).

**Refactor risk**: a future change that derives source pod
name from cluster state at each phase (which would sound
cleaner — "always read fresh state") silently re-introduces
the chain-migration race. The pattern is "lock invariant
identity at one boundary, use consistently downstream."

**Discovered by**: Phase 3a E12 walkthrough (W26, 2026-05-04).

**Tracked in**: PR #53 commit message.

### LBA-3: CH 5-iteration cap guarantees pre-copy termination

**Where**: Cloud Hypervisor v51.1 source (external; not
KubeSwift code). Phase 3b's swift-ch-client and swiftletd
treat the CH RPC as opaque; the cap is internal to CH.

**What**: Cloud Hypervisor v51.1 hardcodes pre-copy to 5
iterations and then **unconditionally** enters final stop-and-
copy. Phase 3b's pre-copy-is-viable claim depends on this
property.

**Why it's load-bearing**: Phase 3b's webhook does NOT gate on
dirty-rate estimation (Section 2.2). Hostile workloads complete
because CH stops iterating, not because the algorithm decided
"good enough." If CH didn't terminate unconditionally, hostile
workloads would never converge and Phase 3b's pass-criteria
would change.

**CH-version dependency**: if upstream makes the cap
configurable, raises it, or removes it (e.g., switches to
classical dirty-rate-vs-bandwidth detection), Phase 3b's
assumptions about hostile-workload completion reshape.
Worst-case current behavior: ~105s migration for 4Gi guest at
50% RAM dirty rate, ~2.6s operator-visible downtime.

**Discovered by**: Phase 2 spike (Decision 4) + Phase 3b spike
Q2 (re-validation under stress-ng).

**Tracked in**: this design doc; Section 12 lists CH-upgrade
discipline as a future-phase prerequisite.

### LBA-4: Default pod network 95% efficiency

**Where**: Phase 3b's swiftletd uses default Calico VXLAN pod
networking for the migration TCP channel. No code; an
environmental property of the deployment.

**What**: Calico VXLAN at MTU 1450 saturates the underlying
NIC at ~95% efficiency. CH's `vm.send-migration` achieves
~107 MB/s on a network with ~113 MB/s raw TCP capacity (spike
Q4). Orchestration overhead is ~5%.

**Why it's load-bearing**: Phase 3b's operator sizing formula
`(guest_RAM × 1.05) / pod_network_bandwidth` documents
expected transfer duration. Operators capacity-plan against
this formula.

**Variability**:

- Other CNI implementations may see different efficiency
  floors. Cilium with eBPF acceleration could be higher;
  Flannel VXLAN could be lower. Spike measurement is
  Calico-specific.
- Cross-zone / cross-region migration (out of scope for Phase
  3b — Section 10) would have fundamentally different
  characteristics; the formula does not apply.
- Larger guests at higher RAM may saturate different
  bottlenecks (cache, memcpy throughput inside CH); 4Gi spike
  scope doesn't validate >8Gi.

**Discovered by**: Phase 3b spike Q4 (2026-05-08).

**Tracked in**: operator runbook (Section 6.4 caveats).

### LBA-5: Annotation surface scoped to state-machine granularity

**Where**: the swiftletd action/status annotation surface,
across the controller and swiftletd codebases. Specifically:

- Controller-side annotation writes:
  [`live_dispatch.go`](../../internal/controller/swiftmigration/live_dispatch.go),
  cancel handlers, idempotency markers.
- swiftletd-side annotation writes:
  [`rust/swiftletd/src/action.rs`](../../rust/swiftletd/src/action.rs)
  status-publishing helpers.

**What**: the surface is intentionally low-frequency (4-6
controller patches + 5-8 swiftletd patches per migration =
~12-17 total). Per-iteration progress or high-frequency
polling is rejected by design.

**Why it's load-bearing**: annotation round-trip latency is
apiserver-bounded (~540ms median per spike Q1). 50 iterations
× 540ms = ~27s of pure overhead would dominate against the
~38s data-transfer body. The surface is correctly scoped; do
not extend it to streaming-data semantics.

**Refactor risk**: a future "let's add per-iteration progress
to the annotation surface — operators want better visibility"
change is the failure mode this LBA prevents. The right
solution to high-frequency progress visibility is a separate
streaming channel (Section 10 — explicitly out of scope for
Phase 3b).

**Discovered by**: Phase 3b spike Q1 (2026-05-08).

**Tracked in**: Section 2.1 + the progress-estimate Section 5.4
explicitly caps at 95% to make heuristic nature evident.

### LBA index summary

| ID | Property | Origin | Scope |
|---|---|---|---|
| LBA-1 | `newDstPod` clone-src guarantees match-tag | Spike Q3 | Phase 3b version discipline |
| LBA-2 | `srcPodLookupName` invariant | Phase 3a W26 | Chain migration correctness |
| LBA-3 | CH 5-iteration cap termination | Spike Q2 | Pre-copy convergence |
| LBA-4 | 95% pod-network efficiency | Spike Q4 | Operator sizing formula |
| LBA-5 | Annotation surface granularity | Spike Q1 | Control plane scope |

---

## 10. Out of scope

Phase 3b's explicit deferrals, extending Phase 3a's deferral
list. Each item below names what's NOT in Phase 3b and why,
with the next-phase target if known.

### 10.1 IP preservation across nodes

Default node-local pod networking changes the guest IP on
cross-node migration. Operators opt in via
`spec.allowIPChange` (Phase 3a precedent). Multi-node L2 work
(Multus, OVN-K layer-2, OVN-K user-defined networks) is
Tracked Follow-up #1 in `kubeswift_context.md`; gates a
separate later phase. Spike Q4 measured Calico VXLAN
specifically — IP preservation requires a different network
configuration that the spike cluster doesn't run.

### 10.2 VFIO/SR-IOV cross-node migration

Unchanged from Phase 3a: the webhook rejects `live` mode for
guests with VFIO or SR-IOV devices. Phase 4+ work; requires a
VFIO release-and-reallocate primitive that doesn't exist in
Cloud Hypervisor upstream (issue #2251 is the upstream
tracking).

### 10.3 mTLS for migration channel

Phase 3b uses plaintext TCP through the pod network. The
Phase 2 PR-A threat-model gating remains: operators must
acknowledge plaintext via the
`kubeswift.io/migration-phase2-unsafe-plaintext: ack`
annotation. Phase 3c+ adds mTLS as a hardening pass; spike
Q4 confirmed plaintext bandwidth is fine, so mTLS is
hardening, not enabling.

### 10.4 Per-iteration progress reporting

Rejected by design per LBA-5. The heuristic progress estimate
(Section 5.4) is the Phase 3b answer to operator demand for
in-flight visibility. Per-iteration progress would require a
separate streaming channel (swiftletd HTTP status endpoint or
upstream CH telemetry) — out of scope.

### 10.5 CPU-feature mismatch detection

Tracked Follow-up #10. The realistic production failure mode
when migrating between heterogeneous nodes is CPU-feature
mismatch (different microarchs expose different KVM-passable
flags), not version skew. Phase 3b ships without detection;
operator runbook discipline (`lscpu` flag uniformity check
before enabling live migration in a node pool) is the
mitigation. Phase 3c may add a `swiftctl migrate --check`
pre-flight that compares source and destination node CPU
flags and warns (not rejects) on mismatch.

### 10.6 vCPU stop-the-world capture

W28 candidate. `observedTransferDuration` (renamed from
`observedPauseWindow`) measures the full vm.send-migration
RPC duration. The actual vCPU stop-the-world window (CH's
final stop-and-copy sub-phase, typically sub-second) is the
operator-relevant "guest frozen" metric and is not separately
surfaced today.

Three plausible paths to capture it (from spike findings):

1. Future CH versions may grow per-phase timing in the RPC
   response. Upstream-dependent; track and adopt if it lands.
2. `swift-ch-client` could probe `vm.info` around the
   stop-and-copy boundary inside the send-migration RPC.
   Requires interleaving reads with the synchronous send
   call; constrained by the W12 swift-ch-client async refactor
   that was a Phase 3b prerequisite (per Phase 3a tracked
   follow-ups).
3. External observer via Tracked Follow-up #1 multi-node L2
   enablement — ping the guest from a third-node sibling pod
   with 50ms intervals and count consecutive lost pings ×
   50ms. Blocked on multi-node L2 prerequisite.

None of the three paths is mature; strict defer to Phase 3c+.

### 10.7 Larger guests (8Gi+, 16Gi+)

Phase 3b validates on 4Gi (Phase 3a default `live-migratable`
SwiftGuestClass). Spike scope was 4Gi; behavior at higher RAM
is extrapolation. No reason to expect qualitative change (CH's
algorithm is RAM-size-agnostic at the iteration level), but
worth empirical confirmation. Phase 3b PR 3 walkthrough may
include one 8Gi run if operator demand emerges; otherwise
larger-guest validation is a tracked follow-up.

### 10.8 Migration during cluster swiftletd rollout

Not detected by validation; operator runbook documents
"complete the swiftletd rollout before triggering live
migrations." The clone-src pod-builder behavior means a live
migration uses the source pod's image; if the source pod is
on an older swiftletd image but the destination node has only
the newest one cached, the image pull may bog down. Same shape
as v0.1.0's cluster CH upgrade discipline.

### 10.9 CSI driver matrix

Phase 3b is Longhorn-only as a deliberate scope choice. The
`live-migration-capable` storage check (Section 3.2) reads
the StorageClass's `parameters.migratable == "true"` attribute
which is Longhorn-specific. Other CSI drivers will surface
their own equivalent attribute over time (Ceph RBD's
`fsType=xfs + accessMode=RWX` is the closest analog; specific
configuration TBD).

Tracked Follow-up #N (TBD): generalize the storage-eligibility
check to a CSI-driver-aware matrix.

### 10.10 Concurrent migrations

Phase 3b's first cut serializes per-source-node — refuses
new SwiftMigration whose source is a node with an in-flight
SwiftMigration. Bandwidth contention as a concurrent-migration
concern is out of scope; spike Q4 measurements were all
single-stream. Future Phase 3c or later may relax this with
explicit bandwidth budgeting.

### 10.11 Cross-zone / cross-region pod networking

Spike cluster is single-DC (Hetzner FSN1). Migration over WAN
would have fundamentally different bandwidth + latency
characteristics; Phase 3b explicitly assumes intra-DC.

---

## 11. Open questions for Phase 3b walkthroughs

Not blocking design doc lock-in; surface during walkthroughs
and resolve as findings:

### 11.1 Progress estimate accuracy under realistic workloads

Spike Q2 used stress-ng (controlled `rand-set` workloads).
Walkthrough Q2-walkthrough-equivalent validates against
operator-representative workloads:

- Postgres pgbench under load.
- Memcached at fill capacity with random GET/SET.
- A web service under HTTP request load.

The progress estimate formula (Section 5.4) is calibrated to
the Q2 baseline (~108 MB/s pod-network bandwidth). If hostile
workloads consistently overshoot the estimate by >20%,
walkthrough findings flag the calibration question for
Section 5.4 refinement.

### 11.2 Cancel-during-receive race

swiftletd destination pod state machine (Section 5.1) must
handle cancel-before-ready cleanly:

- Cancel during `receive-init`: handler exits before CH spawn.
- Cancel during `receive-listening`: close listener, write
  cancelled status.
- Cancel during `receive-active`: CH `vm.receive-migration`
  returns with `MigrationCanceled` after src cancel; clean
  exit.
- Cancel during `receive-complete` (post-cutover): cancel
  ignored (matches LBA / CancelIgnored gate, Section 4.6).

Unit tests cover each transition; walkthrough validates on
real cluster timing (the race window is narrow).

### 11.3 Cluster-bandwidth variability

Spike Q4 measured boba ↔ miles direct (single-hop within the
Hetzner DC). Walkthrough should include at least one
frida-routed scenario if control-plane-traversal happens in
any migration path; capture the bandwidth and orchestration-
overhead ratios for comparison against Q4.

### 11.4 Status field semantic audit (W27-shape recheck)

Per Section 7 gate 6, audit each new status field's docstring
against what the implementation writes on cluster output. The
specific items to confirm:

- `observedTransferDuration` docstring states "full
  vm.send-migration RPC duration" (NOT "vCPU pause window").
- `observedDowntime` docstring states
  "cutoverStep2DispatchedAt → GuestRunning observation" (NOT
  any other framing).
- `failureReasonCode` docstring enumerates all values from
  Section 4.7 and notes which ones are Phase 3b-new.

### 11.5 RBAC sufficiency for new watch resources

Phase 3a's W7 + W8 + W26 pattern: adding a `r.Get` on a new
resource type from inside the controller-runtime cached client
starves the reconcile queue if `list, watch` verbs aren't
granted. Phase 3b PR 2 must audit:

- New watches the controller opens (none expected — Phase 3b
  reuses Phase 3a's labeled-pod watch via `kubeswift.io/
  migration` label).
- Any new `r.Get` calls in new code (Validating phase
  storage-class lookup is the candidate — verify
  StorageClass list/watch is already granted per Phase 3a
  W8 PR #32 fix).

---

## 12. Future phases

Phase 3c+ tentative shape (NOT committed; for context only):

- **mTLS for migration channel.** Compose with Phase 2 S1
  mitigation (URLs from SwiftMigration CR, not pod
  annotations); trust-anchor model TBD. Hardening pass.
- **CPU-feature mismatch detection** via `swiftctl migrate
  --check <guest> --to <node>` pre-flight. Compares source
  and destination node CPU flag sets; warns on mismatch
  (does not reject). Ergonomic addition mirroring Phase 1's
  target-node-Ready check pattern.
- **W28 path-decision and implementation.** Pick one of the
  three plausible paths (Section 10.6) when one matures.
- **Multi-node L2 design doc** (Tracked Follow-up #1)
  becomes the prerequisite for IP-preservation work.
  Capabilities requiring multi-node L2 cluster: live
  migration with IP preservation, offline migration with
  IP preservation, multi-tenancy with cross-node isolation,
  telco/NFV, stateful services with external clients.
- **Larger-guest validation** (Section 10.7) at 8Gi / 16Gi if
  operator demand emerges.
- **CSI driver matrix** generalization (Section 10.9) when
  Ceph RBD or other CSI driver gets a customer ask.

Phase 4+:

- **VFIO release-and-reallocate primitive** (upstream Cloud
  Hypervisor issue #2251 dependency).
- **VFIO/SR-IOV cross-node migration** built on the primitive.

---

## Appendix A — Spike data tables (verbatim)

### A.1 Q1 — annotation round-trip latency (50 iterations × 2 nodes)

| Target | n | min | mean | median | p95 | max |
|---|---|---|---|---|---|---|
| `spike-target-miles` | 50 | 436ms | 567ms | **543ms** | **742ms** | 764ms |
| `spike-target-boba` | 50 | 449ms | 567ms | **537ms** | **730ms** | 747ms |

Real apiserver round-trip after subtracting ~75ms typical
polling overhead: ~465ms median, ~665ms p95.

### A.2 Q2 — pre-copy convergence under stress-ng workload

4Gi RAM Ubuntu Noble guest, RWX+Block on `longhorn-migratable`,
CH v51.1.

| Workload | stress-ng config | total | downtime | pauseWindow | pauseWindow vs baseline |
|---|---|---|---|---|---|
| Baseline (no stress) | — | 57s | 2.14s | **38.20s** | 1.00× |
| LOW | 1 worker × 64M, rand-set | 63s | 2.04s | **45.04s** | 1.18× |
| MED | 2 workers × 256M, rand-set | 88s | 2.62s | **68.36s** | 1.79× |
| HIGH | 4 workers × 512M, rand-set (2GB = 50% of 4Gi RAM) | 105s | 2.56s | **87.46s** | 2.29× |

All four migrations terminated with `phase=Completed`,
`failureReason=` empty. No convergence failure.

### A.3 Q4 — pod-network TCP plumbing (60s sustained iperf3)

| Test | direction | duration | throughput | retransmits |
|---|---|---|---|---|
| T1 | miles → boba (default MTU 1450) | 60s | **902 Mbit/s** | 31 |
| T2 | miles → boba (mss=1400) | 60s | **905 Mbit/s** | 29 |
| T3a | frida → miles (default MTU) | 60s | **902 Mbit/s** | 31 |
| T3b | frida → boba (default MTU) | 60s | **902 Mbit/s** | 30 |
| T4 | boba → miles (default MTU, reverse-symmetry) | 60s | **902 Mbit/s** | 70 |

CH live-migration data path ÷ raw TCP: 107.2 MB/s ÷ 112.75
MB/s = **0.951** (95% efficiency).

---

## Appendix B — References

- Spike findings:
  [`docs/design/live-migration-phase-3b-spike.md`](live-migration-phase-3b-spike.md)
- Phase 3a design:
  [`docs/design/live-migration-phase-3a.md`](live-migration-phase-3a.md)
- Phase 2 design:
  [`docs/design/live-migration-phase-2.md`](live-migration-phase-2.md)
- Phase 1 design:
  [`docs/design/live-migration-phase-1.md`](live-migration-phase-1.md)
- Project context:
  [`kubeswift_context.md`](../../kubeswift_context.md)
- Phase 3a cluster validation:
  [`docs/migration/phase-3a-cluster-validation.md`](../migration/phase-3a-cluster-validation.md)
- Phase 3a operator runbook:
  [`docs/migration/phase-3a.md`](../migration/phase-3a.md)
- Threat model:
  [`docs/design/THREAT-MODEL.md`](THREAT-MODEL.md)

---

End of Phase 3b live migration design doc.
