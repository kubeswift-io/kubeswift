# Guest Lifecycle

This document describes the SwiftGuest lifecycle: phases, conditions, and how pod state maps to status.

## Phases

| Phase | Description |
|-------|-------------|
| **Pending** | Resolution failed or pod not yet scheduled |
| **Scheduling** | Pod Pending (scheduling) |
| **Running** | Pod Running; VM started; GuestRunning=True |
| **Stopped** | VM stopped (runPolicy or explicit stop) |
| **Failed** | Resolution failed, pod Failed, or VM failed |

## Conditions

| Condition | True when |
|-----------|-----------|
| **Resolved** | SwiftGuestClass, SwiftImage, SwiftSeedProfile resolved successfully |
| **ImageReady** | SwiftImage phase=Ready |
| **PodScheduled** | Pod Running or Succeeded |
| **GuestRunning** | swiftletd reported VM running |

## Status mapping (controller)

The SwiftGuest controller maps pod phase to SwiftGuest status:

| Pod phase | SwiftGuest phase |
|-----------|------------------|
| Pending (scheduling) | Scheduling; PodScheduled=False |
| Pending (unschedulable) | Pending; PodScheduled=False with reason |
| Running | Running; PodScheduled=True; nodeName; podRef |
| Failed | Failed; PodScheduled=False with reason |
| Succeeded | Stopped; PodScheduled=True |

## Status reporting (swiftletd)

swiftletd patches SwiftGuest with GuestRunning:

| VM state | GuestRunning | Reason |
|----------|--------------|--------|
| VM running | True | VmRunning |
| VM exited 0 | False | VmStopped |
| VM exited non-zero | False | VmFailed |

## runPolicy

SwiftGuest `spec.runPolicy` controls desired state:

- **Running** — Start and keep VM running
- **Stopped** — Do not start; if running, stop

(Additional policies may be added in future.)

## Related docs

- [SwiftGuest API](../api/swiftguest.md) — Spec and status fields
- [SwiftGuest reconcile](../swiftguest-reconcile.md) — Controller flow
- [Node runtime](node-runtime.md) — swiftletd status reporting
