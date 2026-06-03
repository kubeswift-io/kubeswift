# Phase 3a Live Migration — Operator Reference

> Audience: KubeSwift operators initiating, monitoring, cancelling,
> or debugging live migrations.
>
> Phase 3a ships **mode=live** on the SwiftMigration controller:
> memory and CPU state transfer between nodes without cold-boot.
> Downtime is measured in seconds (vCPU pause window plus brief
> network reachability gap), not the tens of seconds Phase 1
> offline migration requires.
>
> Phase 3a does NOT include mTLS for the migration channel —
> that's Phase 3b. Phase 3a IS production-usable on trusted
> cluster networks where pod-to-pod traffic is not exposed to
> untrusted parties; deployments with stricter requirements
> should wait for Phase 3b.

---

## Overview

`mode=live` on a SwiftMigration:

- Creates a destination launcher pod on the target node with the
  SwiftGuest's disk content already accessible (RWX+Block storage
  for disk-boot guests; kernel-boot guests have no on-disk state
  to transfer).
- Drives Cloud Hypervisor's pre-copy then stop-and-copy memory
  transfer from source CH to destination CH over plaintext TCP
  (mTLS in Phase 3b).
- Atomically swaps the SwiftGuest's canonical pod reference from
  source to destination at the cutover instant.
- Deletes the source pod and resumes the guest on the destination.

When to use it:

- Kernel-boot SwiftGuests (no disk handoff complexity).
- Disk-boot SwiftGuests on RWX+Block storage (PR #35 / W9 path).
  Both `cloneStrategy: copy` and `cloneStrategy: snapshot` work with
  `volumeMode: Block` — W9.x (PR #37) shipped the
  `snapshot.storage.kubernetes.io/allow-volume-mode-change` annotation on the
  cloneSeed VolumeSnapshotContent, so the CSI provisioner permits the
  Filesystem-snapshot → Block-PVC clone (cluster-validated).
- Workloads where multi-second downtime is unacceptable but
  sub-second is not strictly required.

When NOT to use it:

- VFIO/SR-IOV workloads (GPU passthrough, SR-IOV NIC). Upstream
  Cloud Hypervisor constraint #2251 prohibits live migration of
  VFIO state. The webhook rejects `mode=live` on VFIO guests at
  admission. Use `mode=offline` for these workloads.
- Disk-boot guests on RWO storage. The destination pod cannot
  attach the disk while the source pod still holds it; live
  migration's source-running-during-pre-copy semantic is
  incompatible. Use RWX+Block per the storage access mode CRD
  (`docs/design/storage-access-mode.md`) or fall back to
  `mode=offline`.
- Filesystem RWX storage (Longhorn Generic, NFS-based). The
  CRD admission rejects RWX+Filesystem; the SwiftGuest cannot
  exist with this combination. The `liveMigrationCapable`
  recompute fires on every SwiftMigration admission — there's
  nothing to override.

---

## Initiating a migration

Apply a SwiftMigration manifest:

```yaml
apiVersion: migration.kubeswift.io/v1alpha1
kind: SwiftMigration
metadata:
  name: my-guest-to-boba
  namespace: workloads
spec:
  guestRef:
    name: my-guest
  target:
    nodeName: boba
  mode: live
  timeout: 5m   # default; raise for very large guests
```

Or via swiftctl (after PR 2 ships):

```sh
swiftctl migrate my-guest --to boba --mode live
```

`mode: auto` (the default) resolves to `live` for non-VFIO
guests on RWX+Block storage and `offline` otherwise. The
resolved mode is recorded in `status.mode`.

---

## Cancelling a migration — W12 guidance

> **USE `spec.cancelRequested: true` to cancel a live migration.**
>
> **DO NOT use `kubectl delete pod --grace-period=0 --force` on
> the destination pod.**

Force-deleting the destination pod produces a fast-failure
path on the controller side, but the source-side TCP unwind
remains slow internally. **Empirical timing from PR #46
walkthrough Scenario 5** (force-delete dst on
`kubectl delete pod <dst> --grace-period=0 --force`):

- **T+0s**: SwiftMigration transitions to Failed with
  `failureReason=PodTerminated` and message `"destination pod
  <name> disappeared during StopAndCopy"`. The controller's
  outer dst-pod-NotFound check fires immediately on the
  apiserver's pod-deletion event.

- **Up to ~127s** (background, no operator-visible signal):
  source CH's `vm.send-migration` call is synchronous in
  `swift-ch-client` and TCP-retransmits to a peer that no
  longer exists. The src pod's `migration-status: running`
  annotation lingers throughout this window (cosmetic; the
  SwiftMigration is already terminally Failed). Once the
  kernel gives up retransmitting (~127s default), the src
  swiftletd writes `migration-status: failed` with detail
  `send_migration: connection reset`. The SwiftMigration is
  unaffected by this late status write.

The behavior is **better than this doc previously described**
(the prior narrative claimed ~127s slow-failure of the
SwiftMigration phase itself; cluster reality is fast
PodTerminated at T+0s). The src-side TCP unwind is internal
state that doesn't surface as user-visible delay.

`spec.cancelRequested: true` triggers a controller-side cancel
flow:

1. Controller writes the cancel action annotation on the
   destination pod with the cancel ID.
2. swiftletd-on-dst's D1 cancel handler verifies the
   destination CH PID, SIGKILLs the destination CH process.
3. Source CH's TCP send unwinds (TCP RST from the dst kernel).
4. Source swiftletd writes `migration-status=failed` with
   detail `cancelled` and the cancel action ID.
5. Controller observes the terminal status and transitions
   the SwiftMigration to Cancelled.

**Empirical timing from PR #46 walkthrough Scenario 7**
(`spec.cancelRequested=true` mid-StopAndCopy/transferring):
**~27 seconds end-to-end**. This is faster than the force-
delete path (T+0 controller-side, ~127s src-internal unwind)
because the controller's 30s cancel-ack budget triggers a
fallback (force-delete dst pod) when swiftletd's D1 ack
doesn't arrive in time. D1 itself is blocked while
`dispatch_migration_receive` holds the action loop's current-
thread runtime — Phase 2 spike F2.4 finding. The fallback
delivers a clean Cancelled phase with `failureReason=Cancelled`
and message `"destination pod force-deleted; swiftletd cancel
ack timed out (30s budget)"`.

**Why prefer `spec.cancelRequested` despite the 30s fallback?**

- It surfaces a clean `Cancelled` phase (not Failed), with
  `failureReason=Cancelled` for operator-readable audit trail.
- It's the supported, documented mechanism — operators
  shouldn't reach for `kubectl delete --force` as a debugging
  tool.
- The post-cutover behavior is correct: cancel-after-cutover
  sets a `CancelIgnored` condition and lets the migration
  complete normally (operators don't risk destroying an
  already-migrated guest).

The W12 (action-loop blocking) and W20 (fallback fires
because D1 doesn't dispatch in time) limitations both resolve
in Phase 3b when `swift-ch-client` is refactored to async I/O
(cancellable network calls). At that point, D1 will fire
within sub-seconds and `spec.cancelRequested` cancellation
will land in "few seconds" range.

To cancel:

```sh
kubectl patch smig my-guest-to-boba \
  --type merge -p '{"spec":{"cancelRequested":true}}'
```

The patch can be issued at any pre-cutover sub-state. Once
cutover step 1 has crossed (SwiftGuest's `status.podRef.name`
points at the dst pod), cancel-after-the-point sets a
`CancelIgnored` condition on the SwiftMigration but does not
roll back — the migration drives forward to Completed because
the cluster-of-truth pod reference is already destination.
Operators will see `phase: Completed` with
`conditions[type=CancelIgnored, status=True]`.

---

## Operator-visible behavior

### Post-migration pod name change

After a successful live migration, the SwiftGuest's canonical
launcher pod is the **destination pod**, named
`<guest-name>-mig-<short-uid>` (NOT `<guest-name>`). This is
the cluster-of-truth signal at cutover step 1: when
`SwiftGuest.status.podRef.name` flips to the dst pod name, the
migration has crossed the cutover point.

Operators using `swiftctl logs`, `swiftctl console`, or
`swiftctl ssh` are unaffected — the CLI resolves the canonical
pod via `status.podRef` (PR 2 work).

Operators using kubectl directly should query by label rather
than by pod name:

```sh
# AFTER migration this returns the dst pod, not the src pod:
kubectl get pod -l swift.kubeswift.io/guest=my-guest -n workloads

# This will return NotFound after migration:
kubectl logs my-guest -n workloads   # ← old src pod name
```

### Migration-in-progress signal

A non-terminal SwiftMigration carries the
`kubeswift.io/migration-in-progress` annotation on its
SwiftGuest. The annotation's value is the SwiftMigration's
name. Use this to detect "this guest has an active migration":

```sh
kubectl get swiftguest my-guest -n workloads \
  -o jsonpath='{.metadata.annotations.kubeswift\.io/migration-in-progress}'
```

The annotation is removed when the SwiftMigration reaches a
terminal phase (Completed / Failed / Cancelled).

### Phase + phaseDetail vocabulary

`status.phase` values:

- `Pending` — SwiftMigration created, controller hasn't picked
  it up yet.
- `Validating` — webhook + controller pre-flight checks
  (target node Ready, src pod UID stamped, mode resolution).
- `Preparing` — destination pod created and waited until
  Ready.
- `StopAndCopy` — memory transfer in progress. `phaseDetail`
  refines further:
    - `issuing receive on destination`
    - `destination receiving`
    - `issuing send on source`
    - `transferring guest state`
    - `src migration complete; preparing cutover`
    - `cutover: updating canonical pod`
    - `cutover: deleting source pod`
    - `cutover: completing`
- `Resuming` — destination guest health check after cutover.
- `Completed` / `Failed` / `Cancelled` — terminal.

These phaseDetail strings are stable per the §6.4 vocabulary
discipline; operators may script against them.

---

## Failure modes

When `phase: Failed`, `status.failureReason` distinguishes
why:

| failureReason | Cause | Operator action |
|---|---|---|
| `PodTerminated` | Destination pod K8s-terminated mid-flight (drain, node failure, OOM kill, manual delete). The src CH's send call eventually unwound; D2 watchdog on dst confirmed CH abnormal exit. | Investigate destination node health. Re-issue the SwiftMigration once the cause is resolved. |
| `SourcePodReplaced` | Source pod's UID changed mid-flight — typically from `kubectl delete pod` on the source. SwiftGuest's `runPolicy: Running` recreated the pod with a new UID; the migration's source observation was lost. | The new source pod is a fresh boot. Either accept it and re-issue migration, or investigate why the source was deleted. |
| `Timeout` | `spec.timeout` exceeded since `status.startedAt`. Default 5min. Common causes: very large guest with tight default; network blackhole between nodes; CH stuck in pre-copy convergence. | Inspect src/dst pod logs for swiftletd's last reported status. If the migration was making progress, raise `spec.timeout`. If stuck, debug the underlying network/CH issue. |
| `Cancelled` | Destination side wrote `migration-status=failed` with detail `cancelled` (D1 cancel handler), OR an upstream cancel slipped through to the failure path. Phase routes through Failed because the cancel handler's primary cancel path uses the Cancelled phase; this is the defensive classification. | Usually informational — the operator initiated the cancel. |
| `Other` | Anything else: W1 violation (CH state-vs-wire-protocol contradiction), CH internal error (`send_migration: <err>`), unrecognised swiftletd detail. `failureMessage` carries the raw detail. | Read `failureMessage`; consult swiftletd logs on src + dst. May indicate a bug worth a Phase 3a issue. |

Common debug commands:

```sh
# Inspect the migration:
kubectl describe smig my-guest-to-boba -n workloads

# Inspect the pods:
kubectl get pod -l swift.kubeswift.io/guest=my-guest -n workloads
kubectl get pod -l kubeswift.io/migration=my-guest-to-boba -n workloads

# Source/destination launcher logs:
kubectl logs <src-pod> -c launcher -n workloads --tail=200
kubectl logs <dst-pod> -c launcher -n workloads --tail=200

# swiftletd action surface (annotations carry the live state):
kubectl get pod <src-pod> -n workloads \
  -o jsonpath='{.metadata.annotations}' | jq .
```

---

## F2.4 architectural simplification

> Phase 3a's controller observes both source and destination
> pods exclusively via the apiserver/informer surface. There is
> **no controller→swiftletd command channel.**

The controller writes annotations on pods; swiftletd's action
handlers respond to annotation changes; the controller observes
the resulting status annotations via informer events.

This architecture means:

- Network connectivity issues between the controller-manager
  pod and source/destination pods do not affect migration
  orchestration. The controller doesn't talk to swiftletd
  directly.
- Migration coordination scales with apiserver capacity, not
  with controller-to-pod network paths.
- Phase 3b's mTLS work for the migration channel applies only
  to the swiftletd↔swiftletd connection (the actual memory
  transfer between source and destination CH), not to
  controller observability.

This is a deliberate design choice that simplifies Phase 3b's
mTLS scope (one channel, not two) and improves resilience to
network partitions between the control plane and the data
plane. See design doc §5 (controller-runtime integration).

---

## Operational note: stale CRD silently strips new status fields

When upgrading the controller image across releases that add new
SwiftMigration status fields (e.g., the `cutoverStep2DispatchedAt`
field added by W27a in the W27 follow-up), operators must also
refresh the CRD on cluster:

```
kubectl apply -f config/crd/bases/migration.kubeswift.io_swiftmigrations.yaml
```

Or use `make deploy` / `helm upgrade` (both refresh CRDs as part of
the deployment). This pattern applies to **every release that adds
new status fields** across any KubeSwift CRD, not just SwiftMigration.

**Failure mode** if the CRD is stale: apiserver silently strips
unknown status fields without returning an error. Controller logs
show patches succeeding; the field is documented in the CRD types
package; operators see the field permanently empty in
`kubectl get smig -o yaml` output. The W27 cluster validation hit
this on its first run — `cutoverStep2DispatchedAt` was empty despite
the W27a code shipped because the cluster CRD didn't have the field
in its schema.

**Detection**: run `kubectl explain swiftmigration.status` after a
controller upgrade. If the new field is absent from the explain
output, the CRD is stale. The redeploy sequence is the same fix
across all such upgrades.

---

## Limitations and future work

| Limitation | Future resolution |
|---|---|
| Plaintext migration channel (no mTLS) | Phase 3b — sidecar or first-party CH mTLS support |
| W12 cancellation slow-failure path on `kubectl delete --force` | Phase 3b — `swift-ch-client` async refactor |
| No CPU-feature pre-flight check; mismatched microarchs fail at receive | Phase 3b — admission-time CPU compatibility check (§4 spike F12) |
| VFIO/SR-IOV cannot live-migrate | Upstream CH constraint; use `mode=offline` |
| F2 split-brain hazard on RWX storage during pre-copy | Phase 3c — controller-side write-fence orchestration |
| No progress visibility during pre-copy iterations | Phase 5 — swiftletd progress annotations + controller `status.phaseDetail` enrichment |
| 5-iteration CH pre-copy hardcap; high-dirty-rate workloads exit at stop-and-copy boundary | Upstream CH (Phase 5+ issue tracking) |

---

## See also

- `docs/design/live-migration.md` — overall live migration design
- `docs/design/live-migration-phase-3a.md` — Phase 3a controller
  design (state machine, cutover ordering, failure modes)
- `docs/design/live-migration-phase-3a-spike.md` — Phase 3a spike
  findings (cluster-validated empirical evidence)
- `docs/design/live-migration-phase-2.md` — Phase 2 swiftletd
  plumbing reference
- `docs/design/storage-access-mode.md` — RWX+Block storage
  selection
- `docs/migration/troubleshooting.md` — general migration
  debugging
- `docs/migration/phase-2.md` — manual demo path (Phase 2;
  superseded by Phase 3a's controller-driven path)
