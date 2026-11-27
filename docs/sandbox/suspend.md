# Suspend and resume

A running SwiftSandbox can be suspended: its microVM is paused, its memory and
device state are written to its node, and its launcher pod stops. It uses no
memory, CPU or pod while suspended. Resumed, it carries on where it stopped,
on the same node.

```bash
kubectl patch swiftsandbox agent-1 --type=merge -p '{"spec":{"operatingMode":"Suspended"}}'
kubectl patch swiftsandbox agent-1 --type=merge -p '{"spec":{"operatingMode":"Running"}}'
```

| Phase | Meaning |
|---|---|
| `Suspending` | the launcher is pausing the microVM and writing its state |
| `Suspended` | the state is on the node, no launcher runs; `status.suspendedState` says where and how big |
| `Resuming` | a new launcher on that node is restoring it |

A sandbox created with `operatingMode: Suspended` is not started until it is
set to `Running`.

## What is kept

- The workload's memory: processes, open files, the writable root (it is in
  guest memory), Secret values it was given.
- Its identity: name, NetworkPolicy, ServiceAccount, guest address, MAC and
  vsock CID. The launcher pod is new, so its pod IP changes.
- `spec.timeout` counts running time only.

What is not kept:

- Open TCP connections, to and from the guest, and `kubectl exec` sessions.
- The console before the resume: `kubectl logs -c console` starts again at
  the resume.

On resume the guest's wall clock is set to the host's time (it would
otherwise be behind by the whole suspended interval) and host entropy is
added to its pool; the kernel also reseeds its RNG itself (VM Generation ID).
The `Suspended` condition then says `Resumed`, with how far the clock was off,
or `ResumeSyncFailed` if the guest's agent did not answer.

Suspend and resume take a few seconds; pausing and writing the state of a
256 MiB sandbox takes about 50 ms, and its state is about 55 MB on disk.

In [pod mode](pod-mode.md), an agent-sandbox `Sandbox` set to
`operatingMode: Suspended` suspends its microVM this way, and resumes it when
set back to `Running`.

## Requirements and limits

- A kernel whose bridge has `resume-sync`: `kernels/sandbox:6.6.25`,
  `kernels/gpu-sandbox:6.6.15` or later. With an older kernel the sandbox
  keeps running and the condition says `SuspendUnsupported`.
- Not for a sandbox with a GPU, `spec.outputs`, `spec.poolRef` (a warm
  checkout) or `network.egress.credentials`: admission refuses
  `operatingMode: Suspended` for them.
- Same node only. If that node is gone, or the saved state cannot be restored
  (the image's rootfs on the node was rebuilt, a model or artifact tag now
  points elsewhere, Cloud Hypervisor refuses it), the sandbox fails with
  reason `SnapshotLost`.
- The state is kept under `/var/lib/kubeswift/sandbox-suspend/<sandbox uid>/`
  on the node, readable by root only. It holds the guest's memory, Secret
  values included. Deleting the sandbox removes it (a Job on the node); a
  resumed sandbox's state is removed once the guest runs. There is no cap on
  how much suspended state a node holds; watch the nodes' disks.
- A suspend that fails (the snapshot could not be written) resumes the
  microVM: the sandbox keeps running with `SuspendFailed`, and is not tried
  again until the spec changes.

## Troubleshooting

`kubectl get swiftsandbox <name> -o yaml` shows the `Suspended` condition and
`status.suspendedState`. The launcher's log (`kubectl logs <pod> -c launcher`)
has `sandbox_suspended` and `resume_sync` lines.

| Condition reason | Meaning |
|---|---|
| `Suspending` / `Resuming` | in progress |
| `Suspended` | suspended |
| `Resumed` | resumed and its clock reset |
| `ResumeSyncFailed` | resumed, but the guest's clock and entropy were not reset |
| `SuspendUnsupported` | this sandbox cannot be suspended (shape or kernel) |
| `SuspendFailed` | the snapshot failed; the sandbox keeps running |
| `SnapshotLost` | the saved state cannot be restored; the sandbox failed |

## Test

`make sandbox-suspend-test` (`test/sandbox/suspend.sh`).
