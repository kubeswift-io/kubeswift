# Migration — Troubleshooting

| Symptom | Likely cause | Diagnostic / fix |
|---|---|---|
| Webhook rejects with "spec.mode=live is not yet shipped" | Operator submitted `mode: live` in Phase 1 | Use `mode: offline` or `mode: auto`. Live mode lands in Phase 3. |
| Webhook rejects with "another SwiftMigration is already in progress" | A previous migration's `kubeswift.io/migration-in-progress` annotation is stranded on the SwiftGuest (likely from a force-deleted SwiftMigration that bypassed the finalizer) | `kubectl annotate swiftguest <name> kubeswift.io/migration-in-progress-` to clear, then resubmit. |
| Webhook rejects with "VFIO devices ... cross-node migration is not supported in Phase 1" | Guest has `gpuProfileRef` set or a `type: sriov` interface | Phase 1 doesn't ship cross-node GPU/SR-IOV migration. Phase 4+ work. |
| Webhook rejects with "default node-local bridge networking; cross-node migration would change the guest's IP" | Guest is on default networking and target ≠ source | Add `spec.allowIPChange: true` to the SwiftMigration to acknowledge the IP change, OR attach the guest to a multi-node network (Multus + macvlan, OVN-K layer-2). |
| Webhook rejects with "target node ... is cordoned" | Target node has `spec.unschedulable=true` | `kubectl uncordon <target>` or pick a different target. |
| `phase=Failed`, `failureMessage` mentions "insufficient CPU/memory headroom" | Capacity check found the target doesn't fit the guest | `kubectl describe node <target>` to see allocated resources; either pick a different target or free capacity. |
| `phase=Preparing` stuck at "waiting for volume detach" for >60s on Longhorn | The CSI driver is slow to release the VolumeAttachment, OR a stale VA from a prior failed migration on a different node is referencing the same PV | Inspect: `kubectl get volumeattachment | grep <PV>`. If a stale VA exists on an unrelated node, delete it manually. Otherwise wait — Longhorn typically clears within 90s. |
| `phase=Resuming` stuck for ~30s | Normal: the guest VM is cold-booting on the destination node (~17s typical). The controller is not stuck. | Wait. `swiftctl migration describe` includes a UX hint during this window. |
| `phase=Resuming` stuck for >5min | The destination launcher pod failed to start (image pull, scheduling failure, kubelet error) | `kubectl describe pod <guest>` on the destination, or `kubectl logs <guest>` for swiftletd output. |
| `phase=Completed` but the guest's IP changed | Default node-local networking + cross-node migration with `allowIPChange: true` | Expected behaviour. Operators using service discovery should rely on Service selectors, not hardcoded IPs. |
| `phase=Failed reason=MigrationFailed`, message "destination pod scheduled on X, expected Y (atomicity invariant violated)" | A race between the migration controller's combined patch and the SwiftGuest controller landed the pod on the wrong node | Should be unreachable in normal operation. Surface to maintainers; the `client.MergeFrom` patch is supposed to be atomic. |
| SwiftMigration deletion appears stuck | Controller hasn't run cleanup yet — the finalizer (`migration.kubeswift.io/cleanup`) blocks GC until the source-guest annotation is cleared and (pre-cutover) runPolicy is restored | Check controller logs. `kubectl get swiftmigration <name> -o yaml` to see the finalizer. **Do NOT remove the finalizer manually** — let the controller finish; otherwise the source guest may be left annotated + Stopped. |
| Guest left in `runPolicy=Stopped` after a Failed migration | Pre-cutover failure should have restored runPolicy=Running automatically; this is a controller bug if observed | `kubectl patch swiftguest <name> --type merge -p '{"spec":{"runPolicy":"Running"}}'` to recover. File an issue. |

## Useful commands

```bash
# List all in-flight migrations across the cluster.
swiftctl migration list -A

# Detail on a specific migration.
swiftctl migration describe <name>

# Watch progression.
kubectl get swiftmigration <name> -w

# Inspect the source SwiftGuest's annotations to confirm
# migration-in-progress state.
kubectl get swiftguest <guest> -o jsonpath='{.metadata.annotations}' | jq

# Cancel an in-flight migration (the controller's finalizer cleanup
# runs before the resource is GC'd).
swiftctl migration cancel <name>
```

## Reference

- [overview.md](overview.md) — concepts
- [offline-migration.md](offline-migration.md) — operator guide
- [networking-requirements.md](networking-requirements.md) — storage
  and networking constraints
- `docs/design/live-migration.md` — full design
- `docs/design/live-migration-phase-1-spike.md` — empirical baseline
