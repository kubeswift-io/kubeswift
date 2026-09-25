# SwiftGuest Controller Reconcile Flow

The SwiftGuest controller reconciles SwiftGuest resources by resolving references, rendering seed data, creating the pod envelope, and mapping pod state to status.

## Flow

1. **Resolve** – Resolver fetches SwiftGuestClass, SwiftImage, SwiftSeedProfile and produces ResolvedGuest. On failure: set `Resolved=False`, `phase=Failed`, return.

2. **Seed rendering** – When ResolvedGuest has Seed, render userData/metaData/networkData and create Secret `<guest-name>-seed`.

3. **Intent ConfigMap** – Build RuntimeIntent from ResolvedGuest, serialize to JSON, create ConfigMap `<guest-name>-runtime-intent` with key `runtime-intent.json`.

4. **Pod creation** – Build pod with image volume (from SwiftImage preparedArtifact.pvcRef), seed volume (if present), intent ConfigMap volume. Create or update pod.

5. **Status mapping** – Map pod phase to SwiftGuest status:
   - Pod Pending (scheduling) → `phase=Scheduling`; PodScheduled=False
   - Pod Pending (unschedulable) → `phase=Pending`; PodScheduled=False with reason
   - Pod Running → `phase=Running`; PodScheduled=True; nodeName; podRef
   - Pod Failed → `phase=Failed`; PodScheduled=False with reason
   - Pod Succeeded → `phase=Stopped`; PodScheduled=True

## Lifecycle guards

Before pod creation, the controller applies two guards in order:

**Stopped guard** — If `spec.runPolicy=Stopped` and the pod is gone or
completed, the controller sets `phase=Stopped` and returns without
recreating the pod. If the pod is still up, the controller deletes it with
its default grace period, as `swiftctl stop` does: swiftletd turns the
SIGTERM into an ACPI power-off, and the guest is `Stopped` once the pod is
gone. It records a `Stopping` event on the guest, and until then keeps
reporting the pod's status. A pod already terminating is not deleted again.
The controller does not delete the pod while another operation owns it: a
SwiftMigration of the guest that has not finished, a SwiftRestore onto it
that is `Restoring` or `Resuming`, or a SwiftSnapshot of it that is
`Capturing` (or `Uploading` a full-state `includeDisk` capture). It records a
`StopDeferred` event naming that operation and stops the guest once it is
over. An offline migration and a full-state capture set `runPolicy=Stopped`
themselves and delete the pod on their own; they get no `StopDeferred`
event.

**Restart guard** — If `spec.runPolicy=RestartOnFailure` or `Always`
and the pod has failed (or succeeded for Always), the controller:
1. Computes backoff delay: 10s × 2^restartCount, capped at 300s
2. If the backoff has not elapsed since `status.lastRestartTime`,
   requeues with the remaining delay
3. Otherwise deletes the pod, increments `status.restartCount`,
   sets `status.lastRestartTime`, and returns — the next reconcile
   creates a fresh pod

Both guards key on the launcher **pod's terminal state** (Succeeded/Failed),
which means Cloud Hypervisor exited. A guest **reboot is not such an event on
Cloud Hypervisor**: the VM resets in place, so the CH process and the pod survive and the
guest restarts without any controller action (validated 2026-06-09 — the pod
stayed Running with the same CH PID across an in-guest `reboot`, only the
guest's `boot_id` changed). CH exits — and these guards fire — only on guest
shutdown/poweroff or a crash. (KubeSwift passes no `--no-reboot` to keep this
reset-in-place default; CH v51 exited on reboot, churning the pod.)

## Conditions

- **Resolved** – True when resolution succeeded; False with reason when resolution failed.
- **PodScheduled** – True when pod is Running or Succeeded; False when Pending or Failed.

## Admission vs Reconcile

Validation (required refs, runPolicy enum) is enforced by admission webhooks. Cross-resource checks (SwiftImage Ready, reference existence) are enforced at reconcile time by the controller.
