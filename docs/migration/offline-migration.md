# Offline Migration

> The Phase 1 migration mode. The source guest is fully stopped, its
> root-disk PVC is detached on the source node, and the guest is
> recreated on the target node with the same disk content.

## When to use

- You need to move a guest between nodes (rebalancing, manual
  drain, hardware refresh) and seconds of downtime is acceptable.
- The guest uses VFIO (`gpuProfileRef` set, or any
  `interface.type: sriov`) — these workloads can never live-migrate
  and offline is the only path. *Phase 1 only supports same-node
  GPU migration (which is meaningless); cross-node GPU migration is
  Phase 4+ work.*
- The cluster's storage doesn't support live migration's RWX
  requirements.

## State machine

```
Pending → Validating → Preparing → StopAndCopy → Resuming → Completed
              │
              └── Failed | Cancelled
```

| Phase | What it does | Operator-visible signal |
|---|---|---|
| Pending | First reconcile; immediate transition to Validating. | `status.phase=Pending` flickers; usually skipped. |
| Validating | Re-resolves the source guest, the SwiftGuestClass, and the target node. Runs the **manual capacity check**: target's allocatable - sum-of-running-pod-requests must fit the guest's CPU + memory + LauncherMemoryOverheadMiB (512MiB). | `status.phase=Validating`; `Compatible` condition set on completion. |
| Preparing | Patches `kubeswift.io/migration-in-progress` annotation + `runPolicy=Stopped` on the source guest in a single atomic patch. Deletes the source launcher pod with grace=30s. **Dual-poll** for Pod NotFound AND no VolumeAttachment for the per-guest root PV. | `status.phase=Preparing`; `phaseDetail` reports the sub-state ("waiting for source pod termination", "waiting for volume detach"). |
| StopAndCopy | **Single combined patch** of `runPolicy=Running` + `nodeName=target` on the SwiftGuest. The SwiftGuest controller recreates the launcher pod pinned to the target node. Polls for the destination pod's appearance. | `status.phase=StopAndCopy`; `PodScheduling` event. |
| Resuming | Polls the destination SwiftGuest for `GuestRunning=True` AND `primaryIP` discovery. The wait is boot-bound (~17s on warm cache) — *not* a stuck controller. | `status.phase=Resuming`; `phaseDetail` mentions "GuestRunning" wait. |
| Completed | Clears the in-progress annotation; stamps `CompletedAt`; computes `ObservedDowntime`; sets `Ready=True`. | `status.phase=Completed`; `Ready=True`. |

## Timing characteristics

The Phase 1 spike measured these on a real cluster (Longhorn 1.11.1
RWO storage, 10 GiB Ubuntu Noble guest, k0s 1.34, single-replica
controller-manager). Operators on different storage classes will see
different numbers; the table below is the empirical baseline for the
Longhorn + similarly full-copy-CoW class, plus the architectural
upper bound for true CoW drivers.

| Sub-step | Longhorn (RWO full-copy) | Rook Ceph RBD / EBS (RWO CoW) |
|---|---|---|
| `kubectl apply` SwiftMigration → Validating done | <1s | <1s |
| Preparing: pod gone | ~32s (grace=30s + kubelet teardown) | ~32s |
| Preparing: VolumeAttachment GC'd | +13s (Longhorn finalising detach) | <1s |
| StopAndCopy: spec patch + scheduler placement | <2s | <2s |
| StopAndCopy: PV reattach on target | ~5s (Longhorn rebuild) | <1s (CoW reattach) |
| Resuming: VM cold-boot (Ubuntu Noble + cloud-init resume) | ~17s | ~17s |
| **Total observable downtime** | **~70s** | **~25s** |

The detach window dominates Longhorn-class storage. On true CoW
drivers, the bottleneck is VM boot. Memory-heavy guests with
slower-booting OSes will see longer Resuming times.

## Operator commands

```bash
# Migrate to a specific target.
swiftctl migrate db --to worker-3

# On default networking, acknowledge the IP will change.
swiftctl migrate web --to worker-3 --allow-ip-change

# Watch progress.
swiftctl migration list -A
swiftctl migration describe db-migrate-rxk2

# Cancel an in-flight migration. Pre-cutover cancellation restores
# the source guest's runPolicy=Running; post-cutover cancellation
# clears the in-progress annotation but does NOT roll back the
# nodeName patch (the cutover is committed once the source pod was
# deleted).
swiftctl migration cancel db-migrate-rxk2
```

## Troubleshooting

See [troubleshooting.md](troubleshooting.md) for the full table.

The most common confusion: a migration in `Resuming` phase looks
"stuck" but is in fact waiting for the guest VM to boot on the
destination (~17s on warm cache). `swiftctl migration describe`
output during this window includes a hint reminding operators that
this wait is normal.

## Failure handling

| Failure | Behaviour |
|---|---|
| Validating finds insufficient capacity / cordoned target / missing class | `phase=Failed` with `failureMessage`. Source guest untouched. |
| Preparing-before-pod-delete (e.g., conflict annotation found) | `phase=Failed`; runPolicy NOT touched (the conflict check fires before the patch). Source untouched. |
| Preparing-after-pod-delete crashes | Controller restart resumes from re-entry; idempotent. The cutover is committed once Delete(pod) ran. |
| StopAndCopy crash | Idempotent re-entry; the combined patch is a no-op when current state matches. |
| Resuming: GuestRunning never True | Controller stays polling. If the guest fails to boot on the destination, surfaces eventually as a SwiftGuest Failed condition; the SwiftMigration stays in Resuming. Cancel the migration to clear state. |

## Reference

- `docs/design/live-migration.md` — full design (all five phases)
- `docs/design/live-migration-phase-1-spike.md` — empirical findings
  that drove the direct-PVC-reuse decision
- `docs/migration/networking-requirements.md` — storage and network
  attachment requirements
- `docs/migration/troubleshooting.md` — common issues
