# Phase 3b PR 1 — Manual Demo

> Operator-runnable scaffolding for exercising the Phase 3b PR 1
> swiftletd surface end-to-end **without** controller live-mode
> dispatch (that ships in PR 2).

---

## ⚠️ SECURITY BANNER ⚠️

**Phase 3b PR 1 still rides Phase 2's plaintext TCP migration channel.**
Operators MUST NOT route production traffic through this path. The
`kubeswift.io/migration-phase2-unsafe-plaintext: ack` annotation must
be present on both source and destination launcher pods; the scripts
set it for you. Phase 3c+ adds mTLS for production use. See
[`docs/design/THREAT-MODEL.md`](../../../docs/design/THREAT-MODEL.md).

---

## What this demonstrates

End-to-end live migration of a 4Gi RWX+Block Ubuntu Noble guest from
`miles` to `boba` via Cloud Hypervisor's `vm.send-migration` /
`vm.receive-migration` RPCs over the default pod network, with:

- **Pre-dispatch `receive-ready` annotation** emitted by swiftletd on
  the destination pod before it issues `vm.receive-migration` to CH
  (Phase 3b PR 1 Commit C — design doc §5.1).
- **Pre-dispatch `sending` annotation** emitted by swiftletd on the
  source pod before it issues `vm.send-migration` (Commit D —
  design doc §5.2).
- **Heuristic progress estimate** emitted at ~5s intervals during
  the blocking `vm.send-migration` call, capped at 95% (Commit D —
  design doc §5.4).
- **Dual-write of `status.observedTransferDuration` and
  `status.observedPauseWindow`** by the existing W27b stamping path,
  now extended to populate both fields from the same source value
  (Commit E — design doc §3.5).

The demo does **NOT** exercise the controller live-mode dispatch path
(Validating → PreparingLive → StopAndCopyLive). That's Phase 3b PR 2.
The destination pod is hand-crafted by `launch-pods.sh` (raw pod YAML,
NOT a SwiftGuest CR), and the action-trigger annotations are patched
manually by `trigger-migration.sh` — the same hand-driven flow Phase 2
PR-C established.

The companion walkthrough captured the 8-test matrix.

## Prerequisites

- 3-node k0s cluster `miles` + `boba` + `frida` (CP) with the
  `longhorn-migratable` StorageClass present
  (`parameters.migratable: "true"`).
- swiftletd image carrying Phase 3b PR 1 deployed cluster-wide
  via `KUBESWIFT_LAUNCHER_IMAGE` env on `controller-manager`. Run
  `make deploy` against the cluster after the PR merges + CI
  publishes a fresh `sha-<commit>` tag.
- `kubectl` configured against the cluster (operator's note: use
  the cluster kubeconfig at
  `/home/wrkode/code/vmm-kubeswift/dev-tests/kubeswift/kubeswift-cluster.yaml`).

## Usage

```bash
# 1. Bring up source SwiftGuest + raw destination pod.
./launch-pods.sh

# 2. Trigger live migration and watch annotations / metrics.
./trigger-migration.sh

# 3. Tear everything down.
./cleanup.sh
```

Each script is idempotent within reason — re-running after a
mid-flow interrupt requires `cleanup.sh` first to reset state.

## Expected outputs

### `launch-pods.sh`

```
[1/5] namespace phase-3b-pr1-demo created
[2/5] SwiftImage ubuntu-noble Ready (~90s)
[3/5] SwiftSeedProfile + SwiftGuestClass applied
[4/5] SwiftGuest pr1-guest reaches phase=Running on miles (~140s); IP=192.168.99.X
[5/5] Destination pod pr1-guest-dst created on boba, phase=Running

source pod:      pr1-guest         (node=miles)
destination pod: pr1-guest-dst     (node=boba)
guest IP:        192.168.99.X
```

### `trigger-migration.sh` (no-stress baseline)

```
[1/8] ack annotations stamped on both pods
[2/8] receive action dispatched on dst with id=demo-recv-<ts>
[3/8] dst pre-dispatch annotation:    migration-status=receive-ready (~1s)
[4/8] send action dispatched on src with id=demo-send-<ts>, guest_ram_mib=4096
[5/8] src pre-dispatch annotation:    migration-status=sending (~1s)
[6/8] progress estimate samples on src (~5s cadence):
        t+5s:   migration-progress-estimate=12
        t+10s:  migration-progress-estimate=26
        t+15s:  migration-progress-estimate=39
        t+20s:  migration-progress-estimate=52
        t+25s:  migration-progress-estimate=65
        t+30s:  migration-progress-estimate=78
        t+35s:  migration-progress-estimate=92
        t+38s:  migration-progress-estimate=95 (cap held)
[7/8] src terminal annotations:        migration-status=complete
                                       migration-pause-window-ms=38217
[8/8] dst terminal annotations:        migration-status=running

total wall-clock: ~38s
transfer duration: 38.217s   (= migration-pause-window-ms, per Commit D)
```

`migration-pause-window-ms` is the wire annotation key (unchanged from
Phase 2 PR-B / W27b). Commit E's controller stamping reads this value
and dual-writes it to both `status.observedTransferDuration`
(canonical) and `status.observedPauseWindow` (deprecated alias). PR 1
does NOT touch this wire-key name; that rename, if any, is later
cleanup work.

## Troubleshooting

**`receive-ready` annotation never appears on dst:**
- Check that the dst pod's swiftletd container is running and not
  CrashLoopBackOff: `kubectl logs <dst-pod> -c launcher | tail -30`.
- Verify the ack annotation is present: `kubectl get pod <dst-pod> -o
  jsonpath='{.metadata.annotations}'` should include
  `kubeswift.io/migration-phase2-unsafe-plaintext: ack`.
- The intermediate annotation fires in `handle_namespace`'s `Accept`
  branch (Commit C) — if the action loop hasn't picked up the
  receive action, swiftletd hasn't dispatched yet. Check action
  args annotation is well-formed JSON.

**`migration-progress-estimate` never appears on src:**
- The progress emitter requires `guest_ram_mib` in the action args.
  If absent (e.g., operator hand-crafts args without it), swiftletd
  logs at debug and skips emission — no error, no annotation. Check
  `trigger-migration.sh` is setting the field.
- POD_NAMESPACE / POD_NAME env vars must be set via the downward
  API on the launcher container (they should be by default per the
  controller's pod builder).
- If both above check out, look for `progress_estimate_*` log lines
  on the src pod's swiftletd container (debug level — may need to
  `KUBESWIFT_LOG_LEVEL=debug` or equivalent).

**Migration completes but `observedTransferDuration` is empty on the
SwiftMigration CR:**
- This demo does NOT create a SwiftMigration CR — the dual-write
  stamping (Commit E) only fires from the controller's
  `substateSrcCompleted` handler, which is reached only via a
  controller-driven migration. PR 2 wires the controller path; until
  then, observe `migration-pause-window-ms` on the src pod directly.
- Test T6 in the walkthrough doc exercises a Phase 3a offline
  SwiftMigration CR for the dual-write regression check.

## Walkthrough log

After running each test,
fill in the actual observed values. The expected
columns come from the spike Q2 baseline (no-stress 4Gi: ~38s transfer,
MED workload: ~68s) and structured "Observed" columns the operator
fills in. Findings categorization (LOW → tracked follow-up, MEDIUM →
fix in-PR, HIGH → block merge) per Phase 3b implementation gate 5.
