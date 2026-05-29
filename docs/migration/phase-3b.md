# Live Migration (Phase 3b) — Operator Runbook

> Phase 3b lights up `mode: live` for SwiftMigration: guest memory and
> device state stream to the target node while the VM keeps running, with
> sub-3s operator-visible downtime. This runbook is the operator-facing
> companion to the design doc
> [`docs/design/live-migration-phase-3b.md`](../design/live-migration-phase-3b.md).
>
> Phase 3a (offline mode) runbook: [`phase-3a.md`](phase-3a.md).

---

## 1. Quick start

```bash
swiftctl migrate <guest> --to <node>
```

With no `--preferred-mode` flag the mode defaults to `auto`: the
controller live-migrates when the guest is eligible (Section 2) and falls
back to offline otherwise. Read the resolved mode back from the
SwiftMigration:

```bash
kubectl get smig -o wide        # MODE column = status.mode (resolved)
swiftctl migration describe <name>
```

To force a mode:

```bash
swiftctl migrate <guest> --to <node> --preferred-mode live
swiftctl migrate <guest> --to <node> --preferred-mode offline
```

`--preferred-mode` maps to the CRD field `spec.mode`; the controller
writes the resolved mode to `status.mode`.

On KubeSwift's default node-local-bridge networking a cross-node
migration changes the guest IP — add `--allow-ip-change` to acknowledge
(see Section 5).

---

## 2. Is my guest live-eligible?

A guest is eligible for live migration when **all** hold:

- **Storage is live-migratable**: either a kernel-boot guest
  (`spec.kernelRef`, no root-disk PVC), or a disk-boot guest on
  **ReadWriteMany + Block** storage backed by a Longhorn StorageClass
  with `parameters.migratable: "true"`.
- **No VFIO/SR-IOV devices**: no `spec.gpuProfileRef` and no
  `type: sriov` interface. These cannot live-migrate (upstream Cloud
  Hypervisor constraint #2251) — use offline.

Check the resolved storage on the guest:

```bash
kubectl get sg <guest> -o jsonpath='{.status.storage}{"\n"}'
# live-eligible disk-boot example:
# {"accessMode":"ReadWriteMany","volumeMode":"Block","storageClassName":"longhorn-migratable"}
```

Eligibility is computed from the resolved spec at admission/validation
time (not read from status), so it is correct even immediately after a
cluster restore.

**Explicit `--preferred-mode live` for an ineligible guest is rejected**
at admission with a structured error telling you which condition failed.
`--preferred-mode auto` silently resolves such a guest to offline
instead.

---

## 3. Reading migration metrics

Two distinct timings are reported; they mean different things.

| Field (`status.`) | `kubectl get smig` column | Meaning |
|---|---|---|
| `observedDowntime` | `Downtime` | Operator-visible cluster downtime: cutover dispatch → guest healthy on the destination. **This is the SLO-relevant number.** |
| `observedTransferDuration` | `Transfer` | Full `vm.send-migration` RPC: pre-copy iterations + final stop-and-copy + finalize. The guest stays **responsive** through almost all of this; it is a capacity-planning input, not downtime. (Offline migrations leave this empty.) |

`swiftctl migration describe <name>` prints both, with an inline gloss,
and — during an in-flight live transfer — a best-effort
`Progress (estimate): N%`.

Empirical baselines (4 GiB guest, default Calico pod network,
`cloud-hypervisor v51.1`):

| Workload | `Transfer` | `Downtime` |
|---|---|---|
| No stress (cluster-validated, disk-boot RWX+Block) | ~38s | ~2.9s |
| stress-ng MED (2×256M dirtying) | ~68s | ~2.6s |
| stress-ng HIGH (50% of RAM dirtied) | ~87s | ~2.6s |

`Downtime` stays bounded ~2–3s across the whole workload range;
`Transfer` scales with the memory dirty rate. Sizing rule of thumb:

```
expected Transfer ≈ (guest_RAM × 1.05) / pod_network_bandwidth
```

For offline migrations, `Downtime` is the whole window (~36–70s on
Longhorn full-copy, ~25s on true CoW drivers) and `Transfer` is empty.

---

## 4. The progress estimate is a heuristic

`describe` surfaces `kubeswift.io/migration-progress-estimate` (emitted
by swiftletd-source every ~5s during the send RPC). It is computed from a
fixed ~108 MB/s Calico-VXLAN bandwidth baseline (spike Q4) and the guest
RAM size — **not** from Cloud Hypervisor's actual per-iteration progress
(CH v51.1 does not expose that). It is capped at 95% and can over- or
under-predict on hostile memory-dirtying workloads or on non-Calico CNIs.
Treat it as a "something is happening" signal, not a precise ETA. It is
annotation-only and not persisted to status.

---

## 5. Caveats

- **IP change cross-node.** Default node-local-bridge networking does not
  preserve guest IPs across nodes; live and offline cross-node both
  require `--allow-ip-change`. For IP preservation, attach the guest to a
  multi-node network (Multus + macvlan, or OVN-Kubernetes layer-2). See
  Tracked Follow-up #1 in `kubeswift_context.md`.
- **VFIO/SR-IOV is offline-only**, permanently (until upstream CH #2251).
  GPU and SR-IOV-NIC guests are rejected for live and must use offline.
- **CPU-feature mismatch is NOT detected.** The validation pipeline does
  not compare source/destination CPU flags. On heterogeneous-microarch
  clusters a live migration can succeed at transfer time and then fault
  in the guest when it loads an unavailable feature. **Operator
  discipline:** verify `lscpu` flag uniformity across nodes that
  participate in live migration. A `swiftctl migrate --check` pre-flight
  is tracked (Follow-up #10).
- **Complete swiftletd rollouts before live-migrating.** A live migration
  reuses the source pod's launcher image for the destination pod
  (clone-src pod builder); mid-rollout image skew can stall the
  destination image pull.
- **No default `spec.timeout`.** An unset timeout means no runaway gate;
  set `--timeout` (live mode enforces a 60s floor) on workloads where a
  stuck migration must auto-fail. Cancel anytime with
  `swiftctl migration cancel <name>`.

---

## 6. Troubleshooting

- **Stuck in `PreparingLive` / "waiting for destination pod ready":**
  check the destination pod's image-pull status, the migration TCP
  listener bind, and the `kubeswift.io/migration-phase2-unsafe-plaintext: ack`
  annotation. The destination-listener timeout (~60s) drives a
  `DstNeverReady` failure if the source never connects.
- **`Failed`, `failureReason=EligibilityMismatch`:** the guest's storage
  changed to non-live-eligible between submission and reconcile (e.g. the
  PVC rebound to a non-migratable StorageClass). Reconfirm eligibility
  (Section 2) and resubmit, or use `--preferred-mode offline`.
- **`Failed`, `failureReason=RpcError`:** a Cloud Hypervisor error during
  transfer. Tail the source pod's logs for the un-sanitized message:
  `kubectl logs <guest-or-mig-pod> -n <ns>`.
- **`Failed`, `failureReason=DstScheduleFailed` / `DstNeverReady`:** the
  destination pod could not schedule or never reached receive-ready;
  check target-node capacity and the boot-type node label
  (`kubeswift.io/kernel-node=true` for kernel-boot guests).
- **`describe` shows `Resuming` for a while:** expected — Resuming waits
  for the guest to report healthy on the destination (~17s warm cache).
  The controller is not stuck.
- **Resume after offline migration of a previously live-migrated guest:**
  fixed in TFU #18 — offline now resolves the live-renamed pod correctly.
  If you see an offline migration stuck at "waiting for volume detach"
  on an older build, that is the pre-fix symptom.

---

## 7. Validation history

- PR 1 (#61) — swiftletd send/receive + progress emitter + CRD rename;
  manual-demo walkthrough
  [`phase-3b-pr1-walkthrough.md`](phase-3b-pr1-walkthrough.md).
- PR 2 (#64/#65) — controller live-mode integration; walkthrough
  [`phase-3b-pr2-walkthrough.md`](phase-3b-pr2-walkthrough.md).
- PR 3 — this UX surface (`--preferred-mode`, `describe` Transfer +
  progress, `Transfer` printer column, this runbook).
