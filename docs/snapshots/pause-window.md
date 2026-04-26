# Pause Window — Tier B (Local Backend) Memory Snapshots

Capturing memory pauses the source VM until Cloud Hypervisor has
written every byte of guest RAM to the snapshot directory. This page
gives you a model for predicting that pause and reacting to it.

## The curve

Phase 0 spike measurements on Longhorn-backed hostPath storage:

| VM RAM | Pause Window |
|--------|--------------|
| 1 GiB  | 4.4 s        |
| 4 GiB  | 16.7 s       |
| 16 GiB | 45.3 s       |

Linear in RAM size. Slope: **~2.8 s/GiB**. The intercept (~1.6 s) is
the API round-trip plus CH's metadata-write phase; the scaling cost
is the actual page write.

Faster storage scales the slope down proportionally:

| Storage class                         | Approx. slope |
|---------------------------------------|---------------|
| Longhorn HDD-backed (Phase 0 baseline)| 2.8 s/GiB     |
| Longhorn SSD-backed                   | 1.5–2 s/GiB   |
| Local NVMe hostPath                   | 0.3–0.6 s/GiB |
| tmpfs (no actual persistence)         | 0.05 s/GiB    |

For your cluster: take one snapshot of a representative VM, read
`status.observedPauseWindowMs`, divide by VM RAM in GiB.

## What the operator sees

`SwiftSnapshot.status.observedPauseWindowMs` is set on a successful
local capture. The controller publishes it on the underlying launcher
pod via the `kubeswift.io/snapshot-pause-window-ms` annotation; the
status field is the operator-visible mirror.

```
$ swiftctl snapshot describe db-mem-2026-04-26
Name:        db-mem-2026-04-26
Backend:     local
HostPath:    /var/lib/kubeswift/snapshots/default-db-mem-2026-04-26
Phase:       Ready
Pause Window: 16700ms
...
```

## Eviction during the pause window

The launcher pod is unresponsive on the network during the pause —
nothing inside it answers probes, the dnsmasq listener still serves
but the guest doesn't, and any external observer that pings the VM
sees a dropped packet stream. **The pod must NOT be evicted during
this window**, or the snapshot is lost.

KubeSwift's launcher pod has no readiness/liveness probes today (we
verified this). If you add probes in a future PR, make sure they
either tolerate the pause (long timeout) or skip during capture.

For cluster-autoscaler and descheduler: the launcher pod is
annotated `cluster-autoscaler.kubernetes.io/safe-to-evict=false`
during the pause window (the action handler in swiftletd writes
this annotation on entering pause and removes it on exit; commit
6's capture flow). Operators with custom evictors should honor the
same annotation.

## Choosing a deadline

The SwiftSnapshot controller has a wall-clock deadline (default
600s / 10 minutes) that force-fails Capturing if the launcher
hasn't reported `ready` by then. Override per-snapshot via the
`kubeswift.io/snapshot-deadline-seconds` annotation. Sizing
guidance:

| VM RAM   | Recommended deadline (Longhorn HDD) |
|----------|-------------------------------------|
| ≤ 8 GiB  | 600s (default)                      |
| 16 GiB   | 600s (default; ~45s actual)         |
| 64 GiB   | 1200s (~3 min actual + headroom)    |
| 256 GiB  | 3600s (~12 min actual + headroom)   |
| 1 TiB    | Stop. See below.                    |

Beyond a few hundred GiB, "stop the world for 10+ minutes" is rarely
acceptable in production. Consider:

- Splitting large workloads across multiple smaller VMs.
- Using `csi-volume-snapshot` (disk-only, no pause) and accepting
  the loss of in-memory state.
- Live migration (out of scope for Phase 2; tracked separately).

## Why pause at all? Why not iterative capture?

Cloud Hypervisor v51.x doesn't ship live-migration snapshot (precopy
+ postcopy). Phase 0 evaluated this and accepted the pause-and-write
model as the simplest path to a working memory snapshot. A future
phase can add iterative capture once CH lands the upstream support.

## Troubleshooting

**Pause window much longer than expected.** Check `iostat -x 1` on
the source node during capture: the snapshot directory's underlying
disk should be at near-100% utilization for the duration. If it
isn't, you have storage contention — another workload on the same
node is competing for I/O. Workarounds: drain the node (move other
workloads off) before the capture, or schedule the capture during
off-peak hours.

**Snapshot succeeded but `observedPauseWindowMs` is 0.** Either the
launcher pod failed to write the annotation back (check `kubectl
describe pod` — was it OOMKilled?) or the source VM was already
Paused at capture time. The second case is a no-op pause; capture
proceeds but the elapsed time isn't a useful metric.

**SwiftSnapshot stuck in Capturing past the deadline.** The launcher
pod died silently or `swiftletd` is hung on a kernel I/O wait.
Inspect with `kubectl logs <pod> --previous` and `kubectl describe
swiftsnapshot <name>`. The deadline message will name the failure
reason; the standard remediation is delete the SwiftSnapshot
(triggers cleanup) and retry.
