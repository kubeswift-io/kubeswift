# Phase 2 Live Migration — Operator Reference

> **Status:** Test surface, not a production migration mode.

---

## ⚠️ SECURITY BANNER ⚠️

**Phase 2 swiftletd live-migration plumbing carries unauthenticated guest state in cleartext on the cluster network. Operators MUST NOT route production traffic through this path. Phase 2 is a swiftletd-extension test surface; Phase 3 adds mTLS for production use.**

**Routing a production VM through Phase 2 is a security incident.** Full guest memory and CPU state, including any in-memory secrets (TLS private keys, application credentials, kernel keyrings, decrypted disk content held in page cache), is exposed in cleartext to anyone with read access to the cluster pod network for the duration of the migration.

The `kubeswift.io/migration-phase2-unsafe-plaintext: ack` annotation is required on both source and destination launcher pods for swiftletd to accept any migration action. The annotation must be set to the literal string `ack`. This gate is removed in Phase 3 once mTLS lands.

---

## What Phase 2 ships

Phase 2 is the **swiftletd plumbing layer** beneath live migration:

- New `vm.send-migration` and `vm.receive-migration` primitives in `swift-ch-client`
- New `spawn_ch_receive` for destination-side empty CH startup
- An annotation-driven action surface in swiftletd that mirrors snapshot Phase 2's pattern, scoped to the new `kubeswift.io/migration-*` namespace
- A `kubeswift.io/migration-role: receiver` env-var (`KUBESWIFT_MIGRATION_ROLE=receiver`) hint that switches swiftletd into "spawn empty CH and wait for receive-migration" mode
- A manual operator workflow under `test/migration/manual/` that drives all of the above without a controller

Phase 2 does **not** ship:

- Controller-orchestrated live migration (that's Phase 3 — `SwiftMigration.spec.mode: live`)
- mTLS-encrypted migration channel (Phase 3)
- Drain integration / eviction webhook (Phase 4)
- Progress polling thread (`precopy`/`stopcopy`/`listening` annotation transitions) — deferred follow-up; PR-C ships terminal `running` (accept) → `complete` (source) / `running` (destination) only
- OS-thread + tokio::oneshot dispatch model — deferred follow-up; the action loop blocks during migration dispatch
- Action-loop-driven cancel handler — `MigrationCancel` is a placeholder; cancel via `kubectl delete pod` on the destination launcher

For production migration today, use [Phase 1's `SwiftMigration` CRD with `mode: offline`](offline-migration.md). Offline migration produces tens of seconds of downtime instead of milliseconds-to-seconds, but it works for ALL workloads (including VFIO/SR-IOV that can never live-migrate per upstream Cloud Hypervisor #2251).

## When to use Phase 2's manual workflow

Only when you specifically need to test the swiftletd extension end-to-end on a real cluster — for example:

- Validating swiftletd image changes that touch the migration code paths
- Reproducing F1–F12 / W1 / W2 spike findings under controlled conditions
- Pre-flight testing before Phase 3 controller integration

Operators on production clusters should not use this workflow. The unsafe-plaintext-ack gate exists exactly to make accidental production use impossible.

## Prerequisites

- Two-node cluster with both nodes labelled `kubeswift.io/launcher-node=true`. The smoke-test miles + boba pair is the validated configuration.
- The same swiftletd image deployed cluster-wide. Phase 2 mandates exact-image-tag match across source and destination CH (Phase 2 Decision 3 — `spec.allowVersionSkew` lands in Phase 3).
- A running SwiftGuest on the source node with a known sentinel marker file the operator can verify post-migration.
- Operator-level RBAC on the namespace (pods/get, pods/patch, pods/exec, pods/log, swiftguests/get).

## End-to-end workflow

```bash
# 1. Pre-migration: write a sentinel inside the source guest.
#    Record its md5 — you will compare it post-migration.
kubectl exec <source-pod> -c launcher -- swiftctl ssh <swiftguest-name> -- \
    'echo SPIKE-PRE-MIG-$(date +%s) | sudo tee /root/sentinel.txt && sudo md5sum /root/sentinel.txt'

# 2. Run the full demo. SWIFTGUEST is the source guest's name; TARGET_NODE
#    is the destination hostname (must differ from the source pod's node).
make migration-phase2-manual SWIFTGUEST=<name> TARGET_NODE=<node>
```

The Make target chains four scripts:

| Script | Action |
|---|---|
| `source.sh` | Inspects the source SwiftGuest, captures the launcher-pod metadata and YAML |
| `destination.sh` | Renders the destination launcher pod (receiver mode) and applies it |
| `run.sh` | Annotates both pods to trigger receive then send; polls for both terminal statuses (W1 invariant — both gates fire) |
| `verify.sh` | Reads the sentinel file from the migrated guest's serial console |

Compare the post-migration md5 (from `verify.sh`) against the pre-migration md5. They should match — the disk content survived the migration intact.

## Annotation surface (operator-visible)

**Source launcher pod**:

| Key | Set by | Value |
|---|---|---|
| `kubeswift.io/migration-phase2-unsafe-plaintext` | Operator (must) | `ack` |
| `kubeswift.io/migration-action` | Operator (run.sh) | `send` |
| `kubeswift.io/migration-action-id` | Operator (run.sh) | unique ULID |
| `kubeswift.io/migration-action-args` | Operator (run.sh) | JSON: `{"target_url": "tcp:<dst-ip>:6789"}` |
| `kubeswift.io/migration-status` | swiftletd | `running` (accept) → `complete` (success) or `failed` |
| `kubeswift.io/migration-status-id` | swiftletd | echoes the action-id |
| `kubeswift.io/migration-status-detail` | swiftletd | sanitized error category on failure |
| `kubeswift.io/migration-pause-window-ms` | swiftletd | observed migration time in ms |

**Destination launcher pod**:

| Key | Set by | Value |
|---|---|---|
| `kubeswift.io/migration-role` | Pod-builder (destination.sh) | `receiver` (env var `KUBESWIFT_MIGRATION_ROLE=receiver` is the load-bearing signal) |
| `kubeswift.io/migration-phase2-unsafe-plaintext` | Operator (must) | `ack` |
| `kubeswift.io/migration-action` | Operator (run.sh) | `receive` |
| `kubeswift.io/migration-action-id` | Operator (run.sh) | unique ULID |
| `kubeswift.io/migration-action-args` | Operator (run.sh) | JSON: `{"listen_url": "tcp:0.0.0.0:6789"}` |
| `kubeswift.io/migration-status` | swiftletd | `running` (accept and success — same string; verify via vm_info) |
| `kubeswift.io/migration-status-id` | swiftletd | echoes the action-id |
| `kubeswift.io/migration-status-detail` | swiftletd | sanitized error category on failure |
| `kubeswift.io/migration-pause-window-ms` | swiftletd | observed migration time in ms |

The destination's `running` status is ambiguous between accept-time and terminal success. PR-C's `run.sh` resolves the ambiguity by gating on the SOURCE's `complete` status: per Q1c spike finding, the destination CH only auto-resumes after a successful receive completion, so a source `complete` plus an alive destination pod implies the destination's `running` is the terminal success.

## Cancel

To cancel an in-flight migration:

```bash
kubectl delete pod <dst-launcher-pod>
```

PR-B's `MigrationCancel` action handler ships as a placeholder — `kubectl delete pod` on the destination is the operational equivalent. Kubelet sends SIGTERM → swiftletd → CH; on the source, the in-flight `send-migration` returns `connection refused` (F2 finding) and the source CH automatically resumes the guest.

The action-loop-driven cancel SIGKILL handler is deferred to a follow-up PR.

## Failure modes

| Failure | Source state | Destination state | What you'll see |
|---|---|---|---|
| Source crashes mid-migration (F1) | gone | gone (CH self-terminates) | Both pods exit; no terminal status written. Provision fresh src + dst for retry. |
| Destination cancelled / killed (F2) | Running (auto-resumed) | gone | Source: `migration-status: failed, detail: connection_refused`. Source guest continues running. |
| Network drop mid-migration (F3 + F4) | Running (after rule lifted) | gone (listener self-terminates after a few seconds) | Source: `migration-status: failed, detail: connection_refused`. Provision fresh dst. |
| Tap or PVC missing on destination (F5) | Running | bound to fail at receive-migration | Destination: `migration-status: failed, detail: tap_not_found` or similar. Re-create dst pod with correct mounts. |
| CPU feature mismatch post-receive (F12) | exited cleanly | aborts pre-resume | Source: `complete`. Destination: `failed, detail: cpu_incompat`. Pre-flight CPU check is Phase 3 work; for Phase 2 manual demo, ensure source and destination nodes have matching CPU microarch. |
| Stale API socket on destination (W2) | Running | swiftletd-CH startup fails with "Address in use" | PR-A's `rm_stale_api_socket` cleanup prevents this; if the cleanup somehow regressed, dst pod exits without writing a status. |

The W1 completion-gate invariant fires when `send_migration` returns `0` but `vm_info` still reports `state=Running` (source) or reports a non-Running state (destination). Surfaced as `migration-status: failed, detail: w1_violation: ...`. PR-B's regression tests pin this contract.

## Logs and observability

Both launcher containers log under `kubectl logs <pod> -c launcher`. Look for:

```
action_accept namespace=migration kind=MigrationSend id=<ulid>
dispatch_migration_send id=<ulid> target=tcp:<ip>:<port>
dispatch_migration_send_complete id=<ulid> elapsed_ms=<ms>
```

(or `MigrationReceive` / `dispatch_migration_receive*` on the destination side.)

Reject paths log as:

```
action_reject_ack_missing namespace=migration incoming=<ulid> ack_key=kubeswift.io/migration-phase2-unsafe-plaintext
action_reject_concurrent namespace=migration id=<ulid>
action_reject_inflight namespace=migration incoming=<ulid> current=<other-ulid>
```

## What's coming in Phase 3

- mTLS-encrypted migration channel (sidecar pattern OR upstream CH support)
- swiftletd reads URL inputs from the SwiftMigration CR via kube-rs, not from operator-writable annotations (S1 mitigation)
- The five operator-writable migration annotation keys (`migration-action`, `migration-action-id`, `migration-target-url`, `migration-listen-url`, `migration-phase2-unsafe-plaintext`) are DELETED from the surface entirely (§8.2.5 deprecation contract)
- Controller-level CPU-feature pre-flight check in the SwiftMigration validating webhook
- `SwiftMigration.spec.mode: live` wired through PR-B's swiftletd extension
- Audit-event schema for migration phase transitions
- `spec.allowVersionSkew` opt-in flag

## Reference docs

- [`test/migration/manual/README.md`](../../test/migration/manual/README.md) — manual demo script-level details
- [`docs/migration/overview.md`](overview.md) — overall migration concepts (covers Phase 1 offline + Phase 2 live + Phase 3 plans)
