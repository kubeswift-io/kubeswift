# Live Migration Phase 4 — Drain Integration (Operator Guide)

> Status: SHIPPED. `kubectl drain` automatically evacuates SwiftGuest VMs off
> a node instead of killing them — including **GPU guests** (offline, via the
> release-and-reallocate path). SR-IOV NIC passthrough guests still need manual
> handling (NIC reattach on the target is out of scope).

## What it does

`kubectl drain <node>` (and any eviction-API caller — cluster-autoscaler,
node upgrades) now **safely evacuates SwiftGuest VMs**: each guest is
migrated off the node (live where possible), and the eviction is blocked
until the guest is gone. **A VM is never evicted-to-death.**

Three pieces cooperate:

1. **Eviction webhook** (`pods/eviction`, `failurePolicy: Ignore`) — denies
   the eviction of a SwiftGuest launcher pod with `429 TooManyRequests` (so
   drain retries every 5s) and stamps `kubeswift.io/drain-requested:<node>`
   on the SwiftGuest.
2. **Drain controller** — sees the marker, creates a `SwiftMigration`
   (`reason: node-drain`, mode from the guest's `drainPolicy`, target node
   chosen by capacity), and clears the marker once the guest has moved.
   The migration's cutover `Delete`s the source pod (a Delete, not an
   Eviction), so the guest moves and the next drain retry proceeds.
3. **Per-guest PodDisruptionBudget** (`maxUnavailable: 0`) — the **hard
   floor**: it protects the VM even if the webhook is down. Created by the
   SwiftGuest controller for every guest with a launcher pod.

## Per-guest drain policy

Set `spec.migration.drainPolicy` on a SwiftGuest (default `Migrate`):

| Value | On drain |
|---|---|
| `Migrate` (default) | `mode=auto` — live where possible; **offline** for GPU guests (release-and-reallocate) and other non-live-capable guests. Drain succeeds. |
| `LiveMigrate` | live only; if the guest can't live-migrate (GPU, or non-live-capable storage), **deny the drain** rather than incur downtime. |
| `Block` | always **deny the drain**; handle the guest manually. |

`spec.migration.enabled: false` is stronger — it disables migration entirely;
drain denies with a manual-handling message.

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: web-1
spec:
  # ...
  migration:
    enabled: true
    drainPolicy: Migrate   # Migrate | LiveMigrate | Block
```

## GPU guests — auto-evacuated OFFLINE

GPU guests (`gpuProfileRef`) **are** auto-evacuated on drain, via the SwiftGPU
**release-and-reallocate** path: the migration controller reserves matching
GPUs on the target node *before* stopping the source, frees the source GPUs at
cutover, and the guest reboots on the target with its GPUs. GPU migration is
**offline only** (Cloud Hypervisor cannot live-migrate a VFIO device), so:

- `drainPolicy: Migrate` (default) → GPU guest is offline-migrated.
- `drainPolicy: LiveMigrate` → drain is **denied** (GPU can't live-migrate;
  set `Migrate` to allow the offline move).

**GPU-node prerequisites** for a drain *target*:
- The node must be a GPU node (`kubeswift.io/gpu-node=true`) with free GPUs
  matching the guest's `SwiftGPUProfile` (the drain controller's target
  selection enforces this — `SwiftGPUNode.status.vfioReady` + free matching
  GPUs).
- **`vfio-pci` must be loaded** on the target node, persistently —
  `/etc/modules-load.d/vfio.conf` (one line: `vfio-pci`). The GPU-discovery
  DaemonSet reports `vfioReady` from `/sys/bus/pci/drivers/vfio-pci`; a node
  that is not `vfioReady` is never chosen as a target. (gpu-init binds the
  GPU to vfio-pci at guest start; it needs the module loaded.)

## SR-IOV NIC guests — NOT auto-evacuated

Guests with an `sriov` interface **cannot** be auto-migrated: the
release-and-reallocate path handles GPUs only; reattaching an SR-IOV NIC on the
target is out of scope. Under any `drainPolicy`, the eviction webhook denies
their eviction with a manual-handling message and does **not** mark them. To
drain a node with an SR-IOV guest, move or stop it manually first, or
`kubectl drain --force` (which deletes the pod — **the VM is lost**; only do
this if the guest is disposable).

## Deploying

Phase 4 requires the eviction webhook, so deploy with a webhook overlay
(cert-manager required):

```bash
# webhook only:
make deploy-with-webhook
# webhook + live-migration mTLS (Phase 3c):
make deploy-with-webhook-and-mtls
```

The drain controller and the per-guest PDB ship in the controller-manager
and are always active; only the eviction webhook needs the overlay. With the
minimal `make deploy` (no webhook), the **PDB still protects** VMs (drain
stalls safely), but there is no automatic migration — drain a node and the
guest's eviction is blocked by the PDB with no auto-evacuation.

> **Upgrade note (stale-CRD strip).** `spec.migration.drainPolicy` is a new
> CRD field. If you upgrade via a custom pipeline that doesn't re-apply the
> CRDs, the apiserver silently strips it. `make deploy*` / `helm upgrade`
> re-apply the CRDs; verify with
> `kubectl explain swiftguest.spec.migration.drainPolicy`.

## Walking through a drain

```bash
# 1. Drain a worker; SwiftGuest VMs evacuate automatically.
kubectl drain miles --ignore-daemonsets --delete-emptydir-data
#   evicting pod default/web-1
#   error when evicting pod "web-1" (will retry after 5s): admission webhook
#     "veviction.kubeswift.io" denied the request: SwiftGuest "web-1" on node
#     "miles" is being migrated off before eviction; retry
#   ... (the guest live-migrates to boba) ...
#   pod/web-1 evicted
#   node/miles drained

# 2. Watch the auto-created migration.
kubectl get swiftmigration -w        # <guest>-drain-<hash>, reason=node-drain

# 3. Confirm the guest landed on the other node.
kubectl get swiftguest web-1 -o jsonpath='{.status.nodeName}{"\n"}'   # boba

# 4. Done; uncordon.
kubectl uncordon miles
```

## Empirical results (dev cluster, miles/boba, CH v51.1, image sha-04c054d)

PR 5 cluster walkthrough (2026-06-02), kernel-boot guest, default node-local
networking, live-migration mTLS enabled:

| Scenario | Result |
|---|---|
| **Drain → auto-migrate** (`drainPolicy: Migrate`) | `kubectl drain miles` → eviction denied 429 (~6 retries at 5s) → drain controller created `phase4-migrate-drain-96888133` (`reason=node-drain`, resolved **mode=live**, target=boba, `allowIPChange=true`) → guest live-migrated to boba, marker cleared → **`node/miles drained`** (exit 0). **observedDowntime 2.30s**, observedTransferDuration 38.48s. |
| **Block** (`drainPolicy: Block`) | eviction denied `... drainPolicy=Block ... handle this guest manually`; **no marker, no migration**; guest stayed Running on miles; drain stalls (operator `--force`s or moves manually). |
| **Webhook down** (controller scaled to 0) | webhook skipped (`failurePolicy: Ignore`) → the `maxUnavailable:0` PDB denied the eviction (`Cannot evict pod as it would violate the pod's disruption budget`) → drain stalls **safely**, VM protected. |
| **Per-guest PDB** | every guest got a `maxUnavailable:0` PDB (ALLOWED DISRUPTIONS 0), owned by the SwiftGuest, selecting its launcher pod; GC'd with the guest. |

> **Drain SwiftGuest nodes with `--delete-emptydir-data`.** The launcher pod
> uses `emptyDir` volumes (runtime dir, serial socket), so a plain
> `kubectl drain <node>` refuses with *"cannot delete Pods with local storage
> (use --delete-emptydir-data to override)"* **before** the eviction webhook
> ever fires — the guest is not migrated. Always include
> `--delete-emptydir-data --ignore-daemonsets`:
> ```bash
> kubectl drain <node> --ignore-daemonsets --delete-emptydir-data
> ```

## Failure modes

| Mode | Behaviour |
|---|---|
| Webhook up, migratable guest | mark → migrate → cutover deletes src → drain proceeds |
| Webhook **down** (`Ignore`) | admission skipped → the `maxUnavailable:0` PDB still blocks the eviction (429) → drain stalls **safely**; VM protected; no auto-migration (investigate the controller) |
| No schedulable target | webhook keeps denying with a clear message; drain stalls; free capacity or `--force` |
| Migration fails mid-drain | marker stays; `SwiftMigration` shows the failure; webhook keeps denying (VM unharmed — live pre-copy never pauses the source). `kubectl delete swiftmigration <name>` to retry, or handle manually |
| `drainPolicy: Block` / `migration.enabled=false` | deny with a manual-handling message; move the guest manually or `--force` |
| GPU guest, no vfio-ready GPU target | webhook marks it, but the drain controller finds no GPU target with capacity → no migration, drain stalls; free a GPU node or load `vfio-pci` on one |
| SR-IOV NIC guest | deny with a manual-handling message (no marker); not auto-evacuated (NIC reattach out of scope) |
| `drain --dry-run` | webhook denies (shows it would block) but skips the marker patch (`sideEffects: NoneOnDryRun`) |

## Troubleshooting

- **Drain hangs forever, no migration created.** Check the SwiftGuest for a
  `DrainNoTarget` event (`kubectl describe swiftguest <g>`): no peer node has
  capacity. For a GPU guest, "capacity" includes a vfio-ready GPU node with
  free matching GPUs — confirm another GPU node exists, is `vfioReady`
  (`kubectl get swiftgpunode`), and has free GPUs. Free capacity, or `--force`.
- **GPU guest won't drain (SR-IOV).** A guest with an `sriov` interface is not
  auto-evacuated (NIC reattach is out of scope) — handle it manually.
- **Drain hangs, controller is down.** Expected (PDB hard floor). Bring the
  controller back; the drain then auto-migrates. Or `--force` to delete the
  VM (data loss).
- **A guest won't migrate** but should: `kubectl describe swiftmigration
  <guest>-drain-*` for the `Failed` reason (often a capacity or storage
  gate). Delete the migration to retry.

## See also

- Design: [`docs/design/live-migration-phase-4.md`](../design/live-migration-phase-4.md)
- Live migration (Phase 3a/b/c): [`phase-3a.md`](phase-3a.md), [`phase-3b.md`](phase-3b.md), [`phase-3c.md`](phase-3c.md)
